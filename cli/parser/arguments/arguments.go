package arguments

// Argument represents parsed command-line argument (or arguments, in the case
// of an `--option value` pair). This interface is implemented by
// `PositionalArgument` and `options.Option`. The main benefits this interface
// affords us are 1) being able to store `option.Options` and
// `PositionalArguments` in the same slice in a type-safe way, and 2) being
// able to easily `Format` a slice of `Argument`s in order to retrieve the
// string representation of the arguments.
type Argument interface {
	GetValue() string
	Format() []string
}

type PositionalArgument struct {
	Value string
}

func (a *PositionalArgument) GetValue() string {
	return a.Value
}

func (a *PositionalArgument) Format() []string {
	return []string{a.Value}
}

type DoubleDash struct{}

func (a *DoubleDash) GetValue() string {
	return "--"
}

func (a *DoubleDash) Format() []string {
	return []string{a.GetValue()}
}

func FromConcrete[T Argument](args []T) []Argument {
	if len(args) == 0 {
		return nil
	}
	argSlice := make([]Argument, 0, len(args))
	for _, a := range args {
		// `append(a, b...)` only works if `a` and `b` have exactly the same type,
		// even if the slice element types are the same. So we use a loop and append
		// one-by-one, converting each element to `Argument` with each append.
		argSlice = append(argSlice, a)
	}
	return argSlice
}

func ToPositionalArguments(args []string) []Argument {
	if len(args) == 0 {
		return nil
	}
	pos := make([]Argument, 0, len(args))
	for _, arg := range args {
		pos = append(pos, &PositionalArgument{Value: arg})
	}
	return pos
}

func FormatAll[T Argument](args []T) []string {
	if len(args) == 0 {
		return nil
	}
	s := make([]string, 0, len(args))
	for _, arg := range args {
		s = append(s, arg.Format()...)
	}
	return s
}
