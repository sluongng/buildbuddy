package githubauth

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/buildbuddy-io/buildbuddy/server/backends/github"
	"github.com/buildbuddy-io/buildbuddy/server/environment"
	"github.com/buildbuddy-io/buildbuddy/server/interfaces"
	"github.com/buildbuddy-io/buildbuddy/server/tables"
	"github.com/buildbuddy-io/buildbuddy/server/util/authutil"
	"github.com/buildbuddy-io/buildbuddy/server/util/claims"
	"github.com/buildbuddy-io/buildbuddy/server/util/cookie"
	"github.com/buildbuddy-io/buildbuddy/server/util/log"
	"github.com/buildbuddy-io/buildbuddy/server/util/status"
	"github.com/golang-jwt/jwt/v4"
	"github.com/google/uuid"
)

const (
	githubIssuer   = "https://github.com"
	contextUserKey = "auth.githubuser"
	loginPath      = "/login/github/"
	authPath       = "/auth/github/"
	jwtDuration    = 24 * 365 * time.Hour
)

type authenticatedGitHubUser struct {
	Profile     *github.GithubUserResponse
	AccessToken string
}

type githubAuthenticator struct {
	env environment.Env
}

func NewGithubAuthenticator(env environment.Env) *githubAuthenticator {
	return &githubAuthenticator{
		env: env,
	}
}

func (a *githubAuthenticator) Login(w http.ResponseWriter, r *http.Request) error {
	if r.URL.Path != loginPath {
		return status.UnauthenticatedError("not a github login")
	}
	a.handler().HandleLinkRepo(w, r, authPath)
	return nil
}

type token struct {
	GithubUser *github.GithubUserResponse `json:"github_user"`
	Expiry     int64                      `json:"exp"`
}

func (t *token) Valid() error {
	if t.GithubUser == nil {
		return status.InternalError("no github user set")
	}
	if t.GithubUser.Login == "" {
		return status.InternalError("invalid github user")
	}
	return nil
}

func subjectFromGithubUser(u *github.GithubUserResponse) string {
	return "https://github.com/" + u.Login
}

func (a *githubAuthenticator) Auth(w http.ResponseWriter, r *http.Request) error {
	if r.URL.Path != authPath {
		return status.UnauthenticatedError("not a github auth request")
	}

	authToken, err := a.handler().Exchange(r)
	if err != nil {
		return err
	}

	resp, err := github.GetUserInfo(authToken)
	if err != nil {
		return err
	}

	expiration := time.Now().Add(jwtDuration)

	t := &token{GithubUser: resp, Expiry: expiration.Unix()}
	if err := t.Valid(); err != nil {
		return err
	}

	jwt, err := assembleJWT(t)
	if err != nil {
		return err
	}

	guid, err := uuid.NewRandom()
	if err != nil {
		return err
	}
	sessionID := guid.String()

	// OK, the token is valid so we will: store the token in our DB for
	// later & set the login cookie so we know this user is logged in.
	// todo use jwt
	cookie.SetLoginCookie(w, jwt, githubIssuer, sessionID, expiration.Unix())
	sesh := &tables.Session{
		SessionID:   sessionID,
		SubID:       subjectFromGithubUser(resp),
		AccessToken: authToken,
		ExpiryUsec:  expiration.UnixMicro(),
	}
	sesh.RefreshToken = authToken
	authDB := a.env.GetAuthDB()
	if err := authDB.InsertOrUpdateUserSession(r.Context(), sessionID, sesh); err != nil {
		return err
	}
	redirURL := cookie.GetCookie(r, cookie.RedirCookie)
	if redirURL == "" {
		redirURL = "/" // default to redirecting home.
	}
	http.Redirect(w, r, redirURL, http.StatusTemporaryRedirect)
	return nil
}

func (a *githubAuthenticator) handler() *github.OAuthHandler {
	// We always use the read-write github app for login for legacy reasons.
	// Previously, we only offered a read-write app, and customers onboarded to it.
	// Even though login only needs read access, there is no easy way to transfer
	// customers to the read-only app once we added support for it.
	//
	// Using the read-write app does NOT mean that users have to grant write access
	// for login. In GitHub, authorizing and installing a GitHub app are different.
	// When *authorizing*, our read-write app only requests read access to emails.
	// In GitHub terminology, you are authorizing an Oauth app.
	// When *installing*, our read-write app requests write access to certain resources.
	// In GitHub terminology, you are installing a GitHub app.
	//
	// TLDR: The login flow only prompts the user to authorize the app, so the user won't
	// be asked to grant any write permissions, even though it's technically under
	// our read-write app.
	ghApp := a.env.GetGitHubAppService().GetReadWriteGitHubApp()
	return ghApp.OAuthHandler().(*github.OAuthHandler)
}

func (a *githubAuthenticator) AuthenticatedHTTPContext(w http.ResponseWriter, r *http.Request) context.Context {
	issuer := cookie.GetCookie(r, cookie.AuthIssuerCookie)
	if issuer != githubIssuer {
		return r.Context()
	}

	c, authenticatedUser, err := a.authenticateUser(w, r)
	ctx := r.Context()
	if authenticatedUser != nil {
		// Store the user information in the context even if authentication fails.
		// This information is used in the user creation flow.
		ctx = context.WithValue(ctx, contextUserKey, authenticatedUser)
	}
	if err != nil {
		return authutil.AuthContextWithError(ctx, err)
	}
	return claims.AuthContextWithJWT(ctx, c, err)
}

func (a *githubAuthenticator) FillUser(ctx context.Context, user *tables.User) error {
	t, ok := ctx.Value(contextUserKey).(*authenticatedGitHubUser)
	if !ok {
		// WARNING: app/auth/auth_service.ts depends on this status being UNAUTHENTICATED.
		return status.UnauthenticatedError("No user token available to fill user")
	}

	pk, err := tables.PrimaryKeyForTable("Users")
	if err != nil {
		return err
	}

	names := strings.SplitN(t.Profile.Name, " ", 1)
	user.UserID = pk
	user.SubID = subjectFromGithubUser(t.Profile)
	user.FirstName = names[0]
	if len(names) > 1 {
		user.LastName = names[1]
	}
	user.Email = t.Profile.Email
	user.ImageURL = t.Profile.AvatarURL

	return nil
}

func (a *githubAuthenticator) Logout(w http.ResponseWriter, r *http.Request) error {
	cookie.ClearLoginCookie(w)

	// Attempt to mark the user as logged out in the database by clearing
	// their access token.
	jwt := cookie.GetCookie(r, cookie.JWTCookie)
	if jwt == "" {
		return nil
	}
	sessionID := cookie.GetCookie(r, cookie.SessionIDCookie)
	if sessionID == "" {
		return nil
	}

	if authDB := a.env.GetAuthDB(); authDB != nil {
		if err := authDB.ClearSession(r.Context(), sessionID); err != nil {
			log.Errorf("Error clearing user session on logout: %s", err)
		}
	}
	return nil
}

func (a *githubAuthenticator) AuthenticatedUser(ctx context.Context) (interfaces.UserInfo, error) {
	// We don't return directly so that we can return a nil-interface instead of an interface holding a nil *Claims.
	// Callers should be checking err before before accessing the user, but in case they don't this will prevent a nil
	// dereference.
	claims, err := claims.ClaimsFromContext(ctx)
	if err != nil {
		return nil, err
	}
	return claims, nil
}

func (a *githubAuthenticator) SSOEnabled() bool {
	return false
}

func (a *githubAuthenticator) authenticateUser(w http.ResponseWriter, r *http.Request) (*claims.Claims, *authenticatedGitHubUser, error) {
	issuer := cookie.GetCookie(r, cookie.AuthIssuerCookie)
	if issuer != githubIssuer {
		return nil, nil, status.PermissionDeniedError("%s: not a Github authenticated user")
	}

	jwtCookie := cookie.GetCookie(r, cookie.JWTCookie)
	if jwtCookie == "" {
		return nil, nil, status.PermissionDeniedErrorf("%s: no jwt set", authutil.LoggedOutMsg)
	}
	sessionID := cookie.GetCookie(r, cookie.SessionIDCookie)

	authDB := a.env.GetAuthDB()
	if authDB == nil {
		return nil, nil, status.FailedPreconditionError("AuthDB not configured")
	}

	// If the token is corrupt for some reason (not just out of date); then
	// bail.
	ut, err := verifyTokenAndExtractUser(jwtCookie, false /*checkExpiry*/)
	if err != nil {
		return nil, nil, err
	}

	// If the session is not found, bail.
	ctx := r.Context()
	sesh, err := authDB.ReadSession(ctx, sessionID)
	if err != nil {
		log.Debugf("Session not found: %s", err)
		// Clear auth cookies if the session is not found. This allows the login
		// flow to request a refresh token, since otherwise the login flow will
		// assume (based on the existence of this cookie) that a valid session exists with a refresh token already set.
		cookie.ClearLoginCookie(w)
		return nil, &authenticatedGitHubUser{Profile: ut}, status.PermissionDeniedErrorf("%s: session not found", authutil.LoggedOutMsg)
	}

	// Now try to verify the token again -- this time we check for expiry.
	// If it succeeds, we're done! Otherwise we fall through to refreshing
	// the token below.
	if ut, err := verifyTokenAndExtractUser(jwtCookie, true /*=checkExpiry*/); err == nil {
		claims, err := claims.ClaimsFromSubID(ctx, a.env, subjectFromGithubUser(ut))
		return claims, &authenticatedGitHubUser{Profile: ut, AccessToken: sesh.AccessToken}, err
	}

	// WE only refresh the token if:
	//   - there is a valid session
	//   - token is just out of date.
	// Still here? Token needs a refresh. Do that now.
	newToken, err := a.renewToken(ctx, sesh.RefreshToken)
	if err != nil {
		// If we failed to renew the session, then the refresh token is likely
		// either empty or expired. When this happens, clear the session from
		// the DB, since it is no longer usable. Also make sure to clear the
		// Session-ID cookie so that the client is forced to go through the
		// consent screen when they next login, which will let us get a new
		// refresh token from the oauth provider. (Without going through the
		// consent screen, we only get an access token, not a refresh token).
		log.Warningf("Failed to renew token for session %+v: %s", sesh, err)
		cookie.ClearLoginCookie(w)
		if err := authDB.ClearSession(ctx, sessionID); err != nil {
			log.Errorf("Failed to clear session %+v: %s", sesh, err)
		}
		return nil, nil, status.PermissionDeniedErrorf("%s: failed to renew session", authutil.LoggedOutMsg)
	}

	sesh.ExpiryUsec = newToken.Expiry * 1000

	if err := authDB.InsertOrUpdateUserSession(ctx, sessionID, sesh); err != nil {
		return nil, nil, err
	}

	newJwt, err := assembleJWT(newToken)
	if err != nil {
		return nil, nil, err
	}

	cookie.SetLoginCookie(w, newJwt, issuer, sessionID, newToken.Expiry)
	claims, err := claims.ClaimsFromSubID(ctx, a.env, subjectFromGithubUser(newToken.GithubUser))
	return claims, &authenticatedGitHubUser{Profile: newToken.GithubUser, AccessToken: sesh.AccessToken}, err
}

func (a *githubAuthenticator) renewToken(ctx context.Context, authToken string) (*token, error) {
	resp, err := github.GetUserInfo(authToken)
	if err != nil {
		return nil, err
	}
	return &token{GithubUser: resp, Expiry: time.Now().Add(jwtDuration).Unix()}, nil
}

func jwtKeyFunc(token *jwt.Token) (interface{}, error) {
	return []byte(*github.JwtKey), nil
}

func assembleJWT(c jwt.Claims) (string, error) {
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, c)
	tokenString, err := token.SignedString([]byte(*github.JwtKey))
	return tokenString, err
}

func verifyTokenAndExtractUser(jwtString string, checkExpiry bool) (*github.GithubUserResponse, error) {
	parser := jwt.Parser{}
	parsedToken := token{}
	_, err := parser.ParseWithClaims(jwtString, &parsedToken, jwtKeyFunc)
	if err != nil {
		return nil, err
	}
	if checkExpiry {
		if time.Now().Unix() > parsedToken.Expiry {
			return nil, status.InternalErrorf("expired token")
		}
	}
	return parsedToken.GithubUser, nil
}
