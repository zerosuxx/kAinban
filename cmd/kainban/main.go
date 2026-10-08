// Command kainban bootstraps CLI credentials for kAinban's agents into
// Kubernetes Secrets.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/zerosuxx/kainban/internal/auth"
	"github.com/zerosuxx/kainban/internal/kube"
	"github.com/zerosuxx/kainban/internal/tui"
)

// appVersion is set at build time with -ldflags "-X main.appVersion=..."
// (the Dockerfile passes its APP_VERSION build arg).
var appVersion = "dev"

const usage = `Usage:
  kainban auth [flags]          interactive credential setup (TUI)
  kainban auth check [flags]    check credentials non-interactively
  kainban board [flags]         kanban board, s starts a ticket's agent pod
                                (alias: kanban; --file PATH for the board file)
  kainban shell [flags]         start a shell with the stored credentials
  kainban run [flags] -- CMD    run CMD with the stored credentials
  kainban version               print the version

Cluster selection: --kubeconfig or KUBECONFIG (or --context) uses the
kubeconfig; otherwise the in-cluster service account when running in a pod;
otherwise ~/.kube/config.

shell/run read the agent-facing Secrets at start (Claude, GitHub, Copilot,
Gemini env vars; Codex auth.json without refresh token in a writable
CODEX_HOME), so no pod restart is needed after kainban auth.

Flags (auth, auth check, shell, run):
  --namespace NS     namespace for the Secrets (default: POD_NAMESPACE,
                     service account namespace, kubeconfig context, "default")
  --kubeconfig PATH  kubeconfig file
  --context NAME     kubeconfig context
  --live             also validate credentials against vendor APIs

Flags (auth check only):
  --json             print a JSON array instead of a table
  --require-all      fail when a provider's credentials are missing

Exit status of auth check: 0 when every provider is valid or missing,
1 when any is invalid/unknown (or missing with --require-all), 2 on usage
or connection errors.
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

type commonFlags struct {
	namespace, kubeconfig, kubeContext string
	live                               bool
}

func (c *commonFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&c.namespace, "namespace", c.namespace, "namespace for the Secrets")
	fs.StringVar(&c.kubeconfig, "kubeconfig", c.kubeconfig, "kubeconfig file")
	fs.StringVar(&c.kubeContext, "context", c.kubeContext, "kubeconfig context")
	fs.BoolVar(&c.live, "live", c.live, "validate against vendor APIs")
}

func (c *commonFlags) connect() (*kube.Target, error) {
	return kube.Connect(kube.Options{Kubeconfig: c.kubeconfig, Context: c.kubeContext, Namespace: c.namespace})
}

func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	return fs
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	bad := func(format string, a ...any) int {
		if format != "" {
			fmt.Fprintf(stderr, "kainban: "+format+"\n\n", a...)
		}
		fmt.Fprint(stderr, usage)
		return 2
	}
	if len(args) == 0 {
		return bad("")
	}
	switch args[0] {
	case "version", "--version", "-version":
		if len(args) > 1 {
			return bad("version takes no arguments")
		}
		fmt.Fprintln(stdout, appVersion)
		return 0
	case "help", "-h", "--help", "-help":
		fmt.Fprint(stdout, usage)
		return 0
	case "board", "kanban":
		var cf commonFlags
		fs := newFlagSet(args[0])
		cf.register(fs)
		file := fs.String("file", "", "board file")
		if err := fs.Parse(args[1:]); err != nil {
			return bad("%v", err)
		}
		if fs.NArg() > 0 {
			return bad("unexpected arguments: %v", fs.Args())
		}
		return runBoard(ctx, cf, *file, stderr)
	case "shell", "run":
		var cf commonFlags
		fs := newFlagSet(args[0])
		cf.register(fs)
		if err := fs.Parse(args[1:]); err != nil {
			return bad("%v", err)
		}
		if args[0] == "shell" && fs.NArg() > 0 {
			return bad("shell takes no arguments; use run -- CMD")
		}
		if args[0] == "run" && fs.NArg() == 0 {
			return bad("run needs a command: kainban run -- CMD [ARGS]")
		}
		return runWithCredentials(ctx, cf, fs.Args(), stderr)
	case "auth":
	default:
		return bad("unknown command %q", args[0])
	}

	var cf commonFlags
	fs := newFlagSet("auth")
	cf.register(fs)
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, usage)
			return 0
		}
		return bad("%v", err)
	}
	rest := fs.Args()
	if len(rest) == 0 {
		return runAuth(ctx, cf, stderr)
	}
	if rest[0] != "check" {
		return bad("unknown auth subcommand %q", rest[0])
	}

	// Flags may also follow "check"; defaults carry over from above.
	cfs := newFlagSet("auth check")
	cf.register(cfs)
	asJSON := cfs.Bool("json", false, "JSON output")
	requireAll := cfs.Bool("require-all", false, "fail on missing")
	if err := cfs.Parse(rest[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, usage)
			return 0
		}
		return bad("%v", err)
	}
	if cfs.NArg() > 0 {
		return bad("unexpected arguments: %v", cfs.Args())
	}
	return runCheck(ctx, cf, *asJSON, *requireAll, stdout, stderr)
}

func runAuth(ctx context.Context, cf commonFlags, stderr io.Writer) int {
	t, err := cf.connect()
	if err != nil {
		fmt.Fprintf(stderr, "kainban: %v\n", err)
		return 2
	}
	fmt.Fprintf(stderr, "kainban: using %s\n", t.Describe())
	store := kube.NewStore(t.Client, t.Namespace)
	if err := tui.Run(ctx, auth.All(), store, tui.Options{Namespace: t.Namespace, Live: cf.live, AppVersion: appVersion}); err != nil {
		if errors.Is(err, context.Canceled) {
			return 130
		}
		fmt.Fprintf(stderr, "kainban: %v\n", err)
		return 1
	}
	return 0
}

func runCheck(ctx context.Context, cf commonFlags, asJSON, requireAll bool, stdout, stderr io.Writer) int {
	t, err := cf.connect()
	if err != nil {
		fmt.Fprintf(stderr, "kainban: %v\n", err)
		return 2
	}
	store := kube.NewStore(t.Client, t.Namespace)
	results := runChecks(ctx, auth.All(), store, cf.live)
	if asJSON {
		// Keep stdout pure JSON; the target goes to stderr.
		fmt.Fprintf(stderr, "# %s\n", t.Describe())
		err = writeCheckJSON(stdout, results)
	} else {
		fmt.Fprintf(stdout, "Cluster: %s\n\n", t.Describe())
		err = writeCheckTable(stdout, results, time.Now())
	}
	if err != nil {
		fmt.Fprintf(stderr, "kainban: %v\n", err)
		return 2
	}
	return checkExitCode(results, requireAll)
}
