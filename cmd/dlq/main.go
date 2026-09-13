// Command dlq inspects and drains the dead-letter queue.
//
// The operational half of having one. Messages arrive here after failing
// maxReceiveCount times; this is how somebody looks at them, fixes the cause,
// and puts them back.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/regisoliveira/dispute-router/internal/awsx"
	"github.com/regisoliveira/dispute-router/internal/config"
	"github.com/regisoliveira/dispute-router/internal/dlq"
)

const usage = `dlq - inspect and drain the dead-letter queue

  peek    [-n 10]                       show messages without consuming them
  replay  [-n 100] [-max-redrives 3] [-dry-run]
                                        move messages back to the main queue
  purge   -yes                          delete everything, unrecoverably

Fix the cause before replaying. A message put back into an unfixed failure
returns, and a loop that looks like work is worse than a queue that is
visibly stuck.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	if err := run(os.Args[1], os.Args[2:]); err != nil {
		// The flag package has already printed the usage for -h.
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(2)
		}
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run(command string, args []string) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	cfg, err := config.Load(".env")
	if err != nil {
		return err
	}

	awsCfg, err := awsx.Load(ctx, awsx.Config{
		Region:          cfg.AWSRegion,
		Endpoint:        cfg.AWSEndpoint,
		AccessKeyID:     cfg.AWSAccessKey,
		SecretAccessKey: cfg.AWSSecretKey,
	})
	if err != nil {
		return err
	}

	redriver := &dlq.Redriver{
		Client: awsx.SQS(awsCfg, cfg.AWSEndpoint),
		Source: cfg.SQSDLQURL,
		Target: cfg.SQSQueueURL,
	}

	switch command {
	case "peek":
		return peek(ctx, redriver, args)
	case "replay":
		return replay(ctx, redriver, args)
	case "purge":
		return purge(ctx, redriver, args)
	default:
		fmt.Fprint(os.Stderr, usage)
		return fmt.Errorf("unknown command %q", command)
	}
}

func peek(ctx context.Context, r *dlq.Redriver, args []string) error {
	flags := flag.NewFlagSet("peek", flag.ContinueOnError)
	limit := flags.Int("n", 10, "how many messages to show")
	if err := flags.Parse(args); err != nil {
		return err
	}

	// Reported, never used to decide anything: ApproximateNumberOfMessages is
	// approximate, excludes in-flight messages, and lags. Gating on it means a
	// command that finds nothing right after another command looked.
	depth, err := r.Depth(ctx, r.Source)
	if err != nil {
		return err
	}
	fmt.Printf("dead-letter queue holds about %d message(s)\n\n", depth)

	messages, err := r.Peek(ctx, *limit)
	if err != nil {
		return err
	}
	if len(messages) == 0 {
		fmt.Println("  nothing readable right now")
		return nil
	}

	for _, m := range messages {
		age := "unknown age"
		if !m.FirstSeen.IsZero() {
			age = time.Since(m.FirstSeen).Truncate(time.Second).String() + " old"
		}
		fmt.Printf("  %-52s  %s\n", m.Summary(), age)
		fmt.Printf("      delivered %d times, replayed %d times\n", m.Receives, m.Redrives)
	}
	return nil
}

func replay(ctx context.Context, r *dlq.Redriver, args []string) error {
	flags := flag.NewFlagSet("replay", flag.ContinueOnError)
	limit := flags.Int("n", 100, "how many messages to move")
	maxRedrives := flags.Int("max-redrives", 3, "refuse a message already replayed this many times")
	dryRun := flags.Bool("dry-run", false, "report what would move, change nothing")
	if err := flags.Parse(args); err != nil {
		return err
	}

	// No depth check before acting. The count is approximate and briefly reads
	// zero while messages are in flight from another command, so gating on it
	// makes replay silently do nothing straight after a peek.
	stats, err := r.Replay(ctx, *limit, *maxRedrives, *dryRun)
	if err != nil {
		return err
	}
	if stats == (dlq.Stats{}) {
		fmt.Println("nothing on the dead-letter queue to replay")
		return nil
	}

	verb := "replayed"
	if *dryRun {
		verb = "would replay"
	}
	fmt.Printf("%s %d, failed %d\n", verb, stats.Replayed, stats.Failed)
	if stats.Skipped > 0 {
		fmt.Printf("skipped %d already replayed %d or more times\n", stats.Skipped, *maxRedrives)
	}

	if stats.Skipped > 0 {
		fmt.Println("\nskipped messages have been put back and are still on the queue.")
		fmt.Println("they are failing for a reason a replay will not change.")
	}
	if stats.Errors != nil {
		return fmt.Errorf("%d message(s) could not be replayed:\n%w", stats.Failed, stats.Errors)
	}
	return nil
}

func purge(ctx context.Context, r *dlq.Redriver, args []string) error {
	flags := flag.NewFlagSet("purge", flag.ContinueOnError)
	confirmed := flags.Bool("yes", false, "required: this destroys the messages")
	if err := flags.Parse(args); err != nil {
		return err
	}

	depth, err := r.Depth(ctx, r.Source)
	if err != nil {
		return err
	}

	// The only operation here that destroys evidence, so it does not happen by
	// accident or by habit. The count is approximate and said so.
	if !*confirmed {
		return fmt.Errorf("this would permanently delete about %d message(s) and cannot be undone; pass -yes if that is what you want", depth)
	}

	if err := r.Purge(ctx); err != nil {
		return err
	}
	fmt.Println("purged. the queue empties over the next minute or so.")
	return nil
}
