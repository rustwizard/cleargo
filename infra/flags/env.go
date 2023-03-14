package flags

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// BindEnv binds environment variables to the flags.
// Env variable name is upper cased flag name and replaced "-" and "." into "_".
//
// Value set via flag has priority over value set via env variable.
func BindEnv(cmd *cobra.Command) {
	if err := BindEnvToFlagSet(cmd.Flags()); err != nil {
		panic(err)
	}
}

func BindEnvToFlagSet(fs *pflag.FlagSet) error {
	set := make(map[string]bool)
	fs.Visit(func(f *pflag.Flag) {
		set[f.Name] = true
	})

	replacer := strings.NewReplacer("-", "_", ".", "_")
	var flagError error
	fs.VisitAll(func(f *pflag.Flag) {
		if flagError != nil {
			return
		}
		if set[f.Name] {
			return
		}

		envVar := replacer.Replace(strings.ToUpper(f.Name))
		if val := os.Getenv(envVar); val != "" {
			switch f.Value.Type() {
			case "stringArray", "stringSlice":
				vals := strings.Split(val, " ")
				for _, v := range vals {
					if err := fs.Set(f.Name, v); err != nil {
						flagError = fmt.Errorf("cannot set flag [%s] with value [%s] got err: %w", f.Name, v, err)
						return
					}
				}
			default:
				if err := fs.Set(f.Name, val); err != nil {
					flagError = fmt.Errorf("cannot set flag [%s] with value [%s] got err: %w", f.Name, val, err)
					return
				}
			}
		}
	})
	return flagError
}
