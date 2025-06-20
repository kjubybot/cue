// Copyright 2018 The CUE Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime"
	"runtime/pprof"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/text/message"

	"cuelang.org/go/cue"
	"cuelang.org/go/cue/cuecontext"
	"cuelang.org/go/cue/errors"
	"cuelang.org/go/cue/interpreter/embed"
	"cuelang.org/go/cue/stats"
	"cuelang.org/go/internal/core/adt"
	"cuelang.org/go/internal/cueexperiment"
	"cuelang.org/go/internal/encoding"
	"cuelang.org/go/internal/filetypes"
	"cuelang.org/go/internal/httplog"
)

// TODO: commands
//   fix:      rewrite/refactor configuration files
//             -i interactive: open diff and ask to update
//   serve:    like cmd, but for servers
//   get:      convert cue from other languages, like proto and go.
//   gen:      generate files for other languages
//   generate  like go generate (also convert cue to go doc)
//   test      load and fully evaluate test files.
//
// TODO: documentation of concepts
//   tasks     the key element for cmd, serve, and fix

type runFunction func(cmd *Command, args []string) error

// wasmInterp is set when the cuewasm build tag is enbabled.
var wasmInterp cuecontext.ExternInterpreter

func statsEncoder(cmd *Command) (*encoding.Encoder, error) {
	file := os.Getenv("CUE_STATS_FILE")
	if file == "" {
		return nil, nil
	}

	stats, err := filetypes.ParseFile(file, filetypes.Export)
	if err != nil {
		return nil, err
	}

	return encoding.NewEncoder(cmd.ctx, stats, &encoding.Config{
		Stdout: cmd.OutOrStderr(),
		Force:  true,
	})
}

// Stats expands [stats.Counts] with counters obtained from other sources,
// such as the Go runtime. The stats are grouped by category to clarify their source.
type Stats struct {
	// CUE groups stats obtained from the CUE evaluator.
	CUE stats.Counts

	// Go groups stats obtained from the Go runtime.
	Go struct {
		AllocBytes   uint64
		AllocObjects uint64
	}
}

// commandGroup makes cmd runnable in a way that implements commands grouping more subcommands,
// such as `cue mod` or `cue refactor`. Note that the user can't actually run these commands to
// do anything useful, but we need RunE for good error messages, as Cobra itself is lacking:
// https://github.com/spf13/cobra/issues/706
func commandGroup(cmd *cobra.Command) *cobra.Command {
	// If the user ran `cue mod nosuchcommand --nosuchflag`, we run the `cue mod` command
	// because `nosuchcommand` was not a known subcommand.
	// In such a case, do not fail with "unknown flag"; what matters to the user is that
	// `nosuchcommand`, which might have such a flag if it existed, was not found at all.
	// The logic below does not need to parse flags in any case.
	cmd.FParseErrWhitelist.UnknownFlags = true

	name := cmd.Name()
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		stderr := cmd.OutOrStderr()
		if len(args) == 0 {
			fmt.Fprintf(stderr, "%s must be run as one of its subcommands\n", name)
		} else {
			fmt.Fprintf(stderr, "%s must be run as one of its subcommands: unknown subcommand %q\n", name, args[0])
		}
		fmt.Fprintf(stderr, "Run 'cue help %s' for known subcommands.\n", name)
		return ErrPrintedError
	}
	return cmd
}

func mkRunE(c *Command, f runFunction) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		c.Command = cmd

		// Note that the setup code below should only run once per cmd/cue invocation.
		// This is because part of it modifies the global state like cueexperiment,
		// but also because running this twice may result in broken CUE stats or Go profiles.
		// However, users of the exposed Go API may be creating and running many commands,
		// so we can't panic or fail if this setup work happens twice.

		statsEnc, err := statsEncoder(c)
		if err != nil {
			return err
		}
		if err := cueexperiment.Init(); err != nil {
			return err
		}
		var opts []cuecontext.Option
		if wasmInterp != nil {
			opts = append(opts, cuecontext.Interpreter(wasmInterp))
		}
		// Embedding should work with [cuecontext.New] too.
		// Currently that causes an import cycle.
		// See: https://cuelang.org/issue/3613
		opts = append(opts, cuecontext.Interpreter(embed.New()))
		c.ctx = cuecontext.New(opts...)
		// Some init work, such as in internal/filetypes, evaluates CUE by design.
		// We don't want that work to count towards $CUE_STATS.
		adt.ResetStats()

		if cpuprofile := flagCpuProfile.String(c); cpuprofile != "" {
			f, err := os.Create(cpuprofile)
			if err != nil {
				return fmt.Errorf("could not create CPU profile: %v", err)
			}
			defer f.Close()
			if err := pprof.StartCPUProfile(f); err != nil {
				return fmt.Errorf("could not start CPU profile: %v", err)
			}
			defer pprof.StopCPUProfile()
		}

		err = f(c, args)

		// TODO(mvdan): support -memprofilerate like `go help testflag`.
		if memprofile := flagMemProfile.String(c); memprofile != "" {
			f, err := os.Create(memprofile)
			if err != nil {
				return fmt.Errorf("could not create memory profile: %v", err)
			}
			defer f.Close()
			runtime.GC() // get up-to-date statistics
			if err := pprof.WriteHeapProfile(f); err != nil {
				return fmt.Errorf("could not write memory profile: %v", err)
			}
		}

		if statsEnc != nil {
			var stats Stats
			stats.CUE = adt.TotalStats()

			// Fill in the runtime stats, which are cumulative counters.
			// Since in practice the number of allocations isn't fully deterministic,
			// due to the inherent behavior of memory pools like sync.Pool,
			// we support supplying MemStats as a JSON file in the tests.
			var m runtime.MemStats
			if name := os.Getenv("CUE_TEST_MEMSTATS"); name != "" && testing.Testing() {
				bs, err := os.ReadFile(name)
				if err != nil {
					return err
				}
				if err := json.Unmarshal(bs, &m); err != nil {
					return err
				}
			} else {
				runtime.ReadMemStats(&m)
			}
			stats.Go.AllocBytes = m.TotalAlloc
			stats.Go.AllocObjects = m.Mallocs

			statsEnc.Encode(c.ctx.Encode(stats))
			statsEnc.Close()
		}
		return err
	}
}

// rootWorkingDir avoids repeated calls to [os.Getwd] in cmd/cue.
// If we can't figure out the current directory, something is very wrong,
// and there's no point in continuing to run a command.
var rootWorkingDir = func() string { panic("must be replaced by a sync.OnceValue") }

// TODO(mvdan): remove this error return at some point.
// The API could also be made clearer if we want to keep cmd public,
// such as not leaking *cobra.Command via embedding.

// New creates the top-level command.
// The returned error is always nil, and is a historical artifact.
func New(args []string) (*Command, error) {
	// Each call to New resets rootWorkingDir, to support [os.Chdir] calls in between.
	rootWorkingDir = sync.OnceValue(func() string {
		wd, err := os.Getwd()
		if err != nil {
			fmt.Fprintf(os.Stderr, "cannot get current directory: %v\n", err)
			os.Exit(1)
		}
		return wd
	})

	cmd := &cobra.Command{
		Use: "cue",
		// TODO: the short help text below seems to refer to `cue cmd`, like helpTemplate.
		Short: "cue emits configuration files to user-defined commands.",

		// We print errors ourselves in Main, which allows for ErrPrintedError.
		// Similarly, we don't want to print the entire help text on any error.
		// We can explicitly trigger help on certain errors via pflag.ErrHelp.
		SilenceErrors: true,
		SilenceUsage:  true,
	}

	c := &Command{
		Command: cmd,
		root:    cmd,
	}
	c.cmdCmd = newCmdCmd(c)

	addGlobalFlags(cmd.PersistentFlags())

	// Cobra's --help flag shows up in help text by default, which is unnecessary.
	cmd.InitDefaultHelpFlag()
	cmd.Flag("help").Hidden = true

	// "help" is treated as a special command by cobra.
	// We use our own template to be more in control of the structure of `cue help`.
	// Note that we need to add helpCmd as a subcommand first, for cobra to work out
	// the proper help text paddings for additional help topics.
	helpCmd := newHelpCmd(c)
	cmd.AddCommand(helpCmd)
	for _, sub := range helpTopics {
		helpCmd.AddCommand(sub)
	}
	cmd.SetHelpCommand(helpCmd)
	cmd.SetHelpTemplate(helpTemplate)

	for _, sub := range []*cobra.Command{
		c.cmdCmd,
		newCompletionCmd(c),
		newEvalCmd(c),
		newDefCmd(c),
		newExportCmd(c),
		newFixCmd(c),
		newFmtCmd(c),
		newGetCmd(c),
		newImportCmd(c),
		newLoginCmd(c),
		newModCmd(c),
		newRefactorCmd(c),
		newTrimCmd(c),
		newVersionCmd(c),
		newVetCmd(c),

		// Hidden
		newExpCmd(c),
		newAddCmd(c),
		newLSPCmd(c),
	} {
		cmd.AddCommand(sub)
	}

	cmd.SetArgs(args)
	return c, nil
}

// Main runs the cue tool and returns the code for passing to os.Exit.
func Main() int {
	start := time.Now()
	cmd, _ := New(os.Args[1:])
	// CUE_BENCH makes the cue tool act like a `go test -bench=. -benchmem` benchmark,
	// doing all of its work and then only printing a benchmark result line to stdout
	// including the elapsed time, Go allocated bytes, and Go allocations count.
	// This is helpful for benchmarking `cue export` or `cue vet` like one would a Go API
	// without having to write one-off bench_test.go files imitating what the CLI does.
	benchName := os.Getenv("CUE_BENCH")
	if benchName != "" {
		// Don't let anything else be printed to stdout; we're only benchmarking.
		cmd.SetOutput(io.Discard)
	}
	// TODO(mvdan): consider using [os/signal.NotifyContext]
	ctx := httplog.ContextWithAllowedURLQueryParams(
		context.Background(),
		allowURLQueryParam,
	)
	if err := cmd.Run(ctx); err != nil {
		if err != ErrPrintedError {
			errors.Print(os.Stderr, err, &errors.Config{
				Cwd:     rootWorkingDir(),
				ToSlash: testing.Testing(),
			})
		}
		return 1
	}
	if benchName != "" {
		fmt.Printf("Benchmark%s\t", benchName)
		fmt.Printf("%d\t%d ns/op", 1, time.Since(start))
		var memStats runtime.MemStats
		runtime.ReadMemStats(&memStats)
		fmt.Printf("\t%d B/op", memStats.TotalAlloc)
		fmt.Printf("\t%d allocs/op", memStats.Mallocs)
		fmt.Printf("\n")
	}
	return 0
}

type Command struct {
	// The currently active command.
	*cobra.Command

	root *cobra.Command

	// _tool.cue subcommands.
	cmdCmd *cobra.Command

	ctx *cue.Context

	hasErr bool
}

type errWriter Command

func (w *errWriter) Write(b []byte) (int, error) {
	c := (*Command)(w)
	c.hasErr = len(b) > 0
	return c.Command.OutOrStderr().Write(b)
}

// Hint: search for uses of OutOrStderr other than the one here to see
// which output does not trigger a non-zero exit code. os.Stderr may never
// be used directly.

// Stderr returns a writer that should be used for error messages.
// Writing to it will result in the command's exit code being 1.
//
// TODO(mvdan): provide an alternative to write to stderr without setting the exit code to 1.
func (c *Command) Stderr() io.Writer {
	return (*errWriter)(c)
}

// TODO: add something similar for Stdout. The output model of Cobra isn't
// entirely clear, and such a change seems non-trivial.

// Consider overriding these methods from Cobra using OutOrStdErr.
// We don't use them currently, but may be safer to block. Having them
// will encourage their usage, and the naming is inconsistent with other CUE APIs.
// PrintErrf(format string, args ...interface{})
// PrintErrln(args ...interface{})
// PrintErr(args ...interface{})

func (c *Command) SetOutput(w io.Writer) {
	c.root.SetOut(w)
}

func (c *Command) SetInput(r io.Reader) {
	c.root.SetIn(r)
}

// ErrPrintedError indicates error messages have been printed directly to stderr,
// and can be used so that the returned error itself isn't printed as well.
var ErrPrintedError = errors.New("terminating because of errors")

// printError uses cue/errors to print an error to stderr when non-nil.
func printError(cmd *Command, err error) {
	if err == nil {
		return
	}

	// Link x/text as our localizer.
	p := message.NewPrinter(getLang())
	format := func(w io.Writer, format string, args ...interface{}) {
		p.Fprintf(w, format, args...)
	}
	errors.Print(cmd.Stderr(), err, &errors.Config{
		Format:  format,
		Cwd:     rootWorkingDir(),
		ToSlash: testing.Testing(),
	})
}

func (c *Command) Run(ctx context.Context) (err error) {
	// Three categories of commands:
	// - normal
	// - user defined
	// - help
	// For the latter two, we need to use the default loading.
	if err := c.root.ExecuteContext(ctx); err != nil {
		return err
	}
	if c.hasErr {
		return ErrPrintedError
	}
	return nil
}
