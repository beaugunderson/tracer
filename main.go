// Command tracer is an mtr-style continuous traceroute with braille sparklines
// and simultaneous IPv4/IPv6 paths.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/mattn/go-isatty"

	"tracer/internal/trace"
	"tracer/internal/ui"
)

func main() {
	opts := trace.DefaultOptions()
	var report time.Duration
	var mono bool
	flag.DurationVar(&opts.Interval, "i", opts.Interval, "interval between probe rounds")
	flag.DurationVar(&opts.Timeout, "t", opts.Timeout, "timeout before a probe is counted as lost")
	flag.IntVar(&opts.MaxHops, "m", opts.MaxHops, "maximum number of hops")
	flag.BoolVar(&opts.Force4, "4", false, "use IPv4 only")
	flag.BoolVar(&opts.Force6, "6", false, "use IPv6 only")
	flag.BoolVar(&opts.Raw, "r", false, "use raw ICMP sockets (needs root); default datagram sockets need no privileges")
	flag.BoolVar(&mono, "mono", false, "disable the per-row latency color gradient")
	flag.BoolVar(&opts.LookupASN, "z", false, "show each hop's origin AS number (Team Cymru)")
	flag.DurationVar(&report, "report", 0, "run headless for this long, print a plain-text report, and exit")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: %s [flags] <host>\n\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()

	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}
	target := flag.Arg(0)

	if opts.Raw && os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "tracer -r needs raw sockets; run it with sudo (or drop -r to use unprivileged datagram sockets).")
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Resolution blocks until it succeeds (it retries through a dropout), so show
	// what's happening on the same line rather than leaving a blank screen. The
	// in-place escapes only make sense on a terminal; piped/logged stderr gets
	// plain lines instead of literal escape garbage.
	tty := isatty.IsTerminal(os.Stderr.Fd())
	onResolve := func(attempt int, prev error) {
		if attempt == 1 {
			if tty {
				fmt.Fprintf(os.Stderr, "tracer: resolving %s…", target)
			}
			return
		}
		reason := "waiting for network"
		if prev != nil {
			reason = prev.Error()
		}
		if tty {
			fmt.Fprintf(os.Stderr, "\r\033[Ktracer: resolving %s… (attempt %d: %s)", target, attempt, reason)
		} else {
			fmt.Fprintf(os.Stderr, "tracer: resolving %s… (attempt %d: %s)\n", target, attempt, reason)
		}
	}
	sessions, err := trace.Start(ctx, target, opts, onResolve)
	if tty {
		fmt.Fprint(os.Stderr, "\r\033[K") // clear the resolving line
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "tracer: %v\n", err)
		os.Exit(1)
	}

	if report > 0 {
		runReport(ctx, sessions, report)
		return
	}

	p := tea.NewProgram(ui.New(sessions, !mono, opts.LookupASN), tea.WithAltScreen(), tea.WithContext(ctx))
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "tracer: %v\n", err)
		os.Exit(1)
	}
}

func runReport(ctx context.Context, sessions []*trace.Session, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
	fmt.Print(ui.Report(sessions))
}
