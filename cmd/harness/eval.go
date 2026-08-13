package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/mrgeoffrich/deepseek-harness/internal/evals"
	"github.com/mrgeoffrich/deepseek-harness/internal/promptvariant"
	"github.com/mrgeoffrich/deepseek-harness/internal/queue"
	"github.com/mrgeoffrich/deepseek-harness/internal/settings"
)

const evalUsage = `harness eval — measure what a prompt change does to a run

  harness eval run -suite FILE -variants a,b [-n 3] [-judge] [-out report.json]
  harness eval score -report FILE          re-score a finished report from stored events
  harness eval variants                    list the prompt variants this build knows
  harness eval suites                      list the suites built into this binary

An eval publishes each task once per variant per replicate onto the WORK
stream, so runs are claimed by the same workers serving everything else, and
scores them from their stored events. Costs real tokens.`

func runEval(ctx context.Context, args []string) error {
	if len(args) == 0 {
		fmt.Println(evalUsage)
		return nil
	}
	switch args[0] {
	case "run":
		return runEvalRun(ctx, args[1:])
	case "score":
		return runEvalScore(ctx, args[1:])
	case "variants":
		return runEvalVariants()
	case "suites":
		return runEvalSuites()
	case "-h", "-help", "--help", "help":
		fmt.Println(evalUsage)
		return nil
	default:
		return fmt.Errorf("unknown eval subcommand %q\n\n%s", args[0], evalUsage)
	}
}

func runEvalVariants() error {
	for _, name := range promptvariant.Names() {
		fmt.Printf("%-20s %s\n", name, promptvariant.Description(name))
		if policy := promptvariant.ReminderPolicyFor(name); policy != "" {
			fmt.Printf("%-20s   reminders: %s\n", "", promptvariant.ReminderPolicyDescription(policy))
		}
	}
	return nil
}

func runEvalSuites() error {
	for _, suite := range evals.EmbeddedSuites() {
		fmt.Printf("%-12s %d tasks  %s\n", suite.Name, len(suite.Tasks), suite.Description)
	}
	return nil
}

func runEvalRun(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("eval run", flag.ContinueOnError)
	suitePath := fs.String("suite", "", "a built-in suite's name, or a path to a suite JSON file (required)")
	variantList := fs.String("variants", "", "comma-separated prompt variants to compare, baseline first (required)")
	replicates := fs.Int("n", 3, "runs per task per variant")
	concurrency := fs.Int("concurrency", 2, "runs in flight at once")
	useJudge := fs.Bool("judge", false, "score each transcript with a model as well as the counters")
	judgeModel := fs.String("judge-model", "", "model the judge uses (config default otherwise)")
	maxSubTurns := fs.Int("max-sub-turns", 0, "override every task's sub-turn budget; applies to all arms at once")
	model := fs.String("model", "", "override the model every task runs on; applies to all arms at once")
	effort := fs.String("effort", "", "override the reasoning effort every task runs at; applies to all arms at once")
	note := fs.String("note", "", "one line on what this run is asking, shown beside it later")
	timeout := fs.Duration("timeout", 30*time.Minute, "how long one run may take")
	out := fs.String("out", "", "write the full report as JSON to this path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *suitePath == "" || *variantList == "" {
		return errors.New("usage: harness eval run -suite FILE -variants a,b [flags]")
	}

	suite, err := resolveSuite(*suitePath)
	if err != nil {
		return err
	}
	variants := splitList(*variantList)
	if err := evals.ValidateVariants(variants); err != nil {
		return err
	}

	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	st, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer st.Close()

	nc, js, err := queue.Connect(cfg.NATSURL)
	if err != nil {
		return err
	}
	defer nc.Close()

	opts := evals.Options{
		Suite:       suite,
		Variants:    variants,
		Replicates:  *replicates,
		Concurrency: *concurrency,
		MaxSubTurns: *maxSubTurns,
		Model:       *model,
		Effort:      *effort,
		Timeout:     *timeout,
		Recorder:    st,
		Note:        *note,
		Progress: func(r evals.Run) {
			status := r.Status
			if r.Err != "" {
				status = "error: " + r.Err
			}
			fmt.Printf("  %-24s %-14s rep %d  %s\n", r.TaskID, r.Variant, r.Replicate, status)
		},
	}
	if *useJudge {
		res := settings.NewResolver(st)
		rec := newHTTPLogRecorder(cfg)
		defer closeHTTPLog(rec)
		model := *judgeModel
		if model == "" {
			model, err = res.String(ctx, settings.KeyDefaultModel)
			if err != nil {
				return fmt.Errorf("resolve %s: %w", settings.KeyDefaultModel, err)
			}
		}
		opts.Judge = &evals.Judge{
			Client: withHTTPLog(cfg, rec, deepSeekAPIKeyProvider(res)),
			Model:  model,
		}
	}

	total := *replicates * len(suite.Tasks) * len(variants)
	fmt.Printf("suite %s: %d runs (%d tasks × %d variants × %d replicates), %d at a time\n",
		suite.Name, total, len(suite.Tasks), len(variants), *replicates, *concurrency)
	if *model != "" {
		fmt.Printf("sessions on %s\n", *model)
	}
	if opts.Judge != nil {
		fmt.Printf("judged by %s\n", opts.Judge.Model)
	}
	fmt.Println()

	report, err := evals.Execute(ctx, publisher{js}, st, opts)
	if err != nil {
		return err
	}

	fmt.Println()
	fmt.Printf("eval run %s\n\n", report.EvalRunID)
	evals.WriteTable(os.Stdout, report)
	if *out != "" {
		if err := writeReport(*out, report); err != nil {
			return err
		}
		fmt.Printf("\nreport written to %s\n", *out)
	}
	return nil
}

// runEvalScore recomputes a report's metrics from the sessions it names.
// Scoring is pure, so a metric added after a run still applies to it — the
// tokens are already spent and the events are still there.
func runEvalScore(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("eval score", flag.ContinueOnError)
	reportPath := fs.String("report", "", "path to a report written by `harness eval run` (required)")
	out := fs.String("out", "", "write the re-scored report here (default: overwrite the input)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *reportPath == "" {
		return errors.New("usage: harness eval score -report FILE")
	}

	b, err := os.ReadFile(*reportPath)
	if err != nil {
		return err
	}
	var report evals.Report
	if err := json.Unmarshal(b, &report); err != nil {
		return fmt.Errorf("parse report: %w", err)
	}

	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	st, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer st.Close()

	// A rescore is silently partial when a member's session has been deleted,
	// so the count says how many it could actually reach.
	rescored := 0
	for i := range report.Runs {
		run := &report.Runs[i]
		if run.SessionID == "" {
			continue
		}
		events, err := st.GetEvents(ctx, run.SessionID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  %s: %v\n", run.SessionID, err)
			continue
		}
		run.Scores = evals.Score(events)
		rescored++
		if report.EvalRunID != "" {
			if err := st.UpdateEvalMember(ctx, evals.StoreMember(report.EvalRunID, *run)); err != nil {
				fmt.Fprintf(os.Stderr, "  %s: record rescore: %v\n", run.SessionID, err)
			}
		}
	}
	fmt.Printf("re-scored %d of %d runs\n\n", rescored, len(report.Runs))
	evals.WriteTable(os.Stdout, &report)

	dest := *out
	if dest == "" {
		dest = *reportPath
	}
	return writeReport(dest, &report)
}

// resolveSuite takes a built-in suite's name or a path to one. A name is
// tried first so `-suite search` works inside the container, where the repo's
// files are not present.
func resolveSuite(nameOrPath string) (*evals.Suite, error) {
	if suite, err := evals.EmbeddedSuite(nameOrPath); err == nil {
		return suite, nil
	}
	return evals.LoadSuite(nameOrPath)
}

func writeReport(path string, report *evals.Report) error {
	b, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

// publisher adapts the JetStream handle to the evals package's seam, which is
// the same shape the HTTP server's RunPublisher uses: one validated request
// onto the WORK stream, and no other reach into the queue.
type publisher struct{ js jetstream.JetStream }

func (p publisher) Publish(ctx context.Context, req queue.Request) error {
	pubCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return queue.PublishRequest(pubCtx, p.js, req)
}

func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}
