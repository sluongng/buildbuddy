package main

import (
	"context"
	"flag"
	"log"
	"os"

	"github.com/buildbuddy-io/buildbuddy/server/util/grpc_client"
	"github.com/buildbuddy-io/buildbuddy/server/util/status"
	"github.com/mattn/go-isatty"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	repb "github.com/buildbuddy-io/buildbuddy/proto/remote_execution"
)

func writeToStdout(b []byte) {
	os.Stdout.Write(b)
	// Print a trailing newline if there isn't one already, but only if stdout
	// is a terminal, to avoid incorrect digest computations e.g. when piping to
	// `sha256sum`
	if (len(b) == 0 || b[len(b)-1] != '\n') && isatty.IsTerminal(os.Stdout.Fd()) {
		os.Stdout.Write([]byte{'\n'})
	}
}

func printMessage(msg proto.Message) {
	out, _ := protojson.MarshalOptions{Multiline: true}.Marshal(msg)
	writeToStdout(out)
}

var target = flag.String("target", "grpcs://remote.buildbuddy.io", "Cache grpc target, such as grpcs://remote.buildbuddy.io")

func main() {
	conn, err := grpc_client.DialTarget(*target)
	if err != nil {
		log.Fatalf(status.Message(err))
	}

	ctx := context.Background()
	caps, err := repb.NewCapabilitiesClient(conn).GetCapabilities(ctx, &repb.GetCapabilitiesRequest{})
	if err != nil {
		log.Fatalf(status.Message(err))
	}

	printMessage(caps)
}
