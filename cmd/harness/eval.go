package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/mrgeoffrich/deepseek-harness/internal/config"
	"github.com/mrgeoffrich/deepseek-harness/internal/evals"
	"github.com/mrgeoffrich/deepseek-harness/internal/promptvariant"
	"github.com/mrgeoffrich/deepseek-harness/internal/settings"
	"github.com/mrgeoffrich/deepseek-harness/internal/store"
)

const evalUsage = `harness eval — measure what a prompt change does to a run

  harness eval run -suite FILE -variants a,b [-n 3] [-judge] [-out report.json]
  harness eval score -report FILE          re-score a finished report from stored events
  harness eval variants                    list the prompt variants this build knows
  harness eval suites                      list the suites built into this binary
  harness eval close <id> [status]         close out a run stranded by a dead orchestrator

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
	case "close":
		return runEvalClose(ctx, args[1:])
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

// runEvalClose finishes a run whose orchestrator died — the terminal was
// closed, or the container was rebuilt under it. The members it did finish
// keep their scores; only the run's own status is stale.
func runEvalClose(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: harness eval close <eval-run-id> [status]")
	}
	status := store.EvalStatusCancelled
	if len(args) > 1 {
		status = args[1]
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

	run, err := st.GetEvalRun(ctx, args[0])
	if err != nil {
		return err
	}
	if run.FinishedAt != nil {
		return fmt.Errorf("eval run %s already finished as %q", run.ID, run.Status)
	}
	if err := st.FinishEvalRun(ctx, run.ID, status, time.Now().UTC()); err != nil {
		return err
	}
	fmt.Printf("closed %s as %s\n", run.ID, status)
	return nil
}

// runEvalRun asks the server to start an eval and then follows it. The verb
// has one implementation and it lives in `harness serve`
// (cmd/harness/stop.go makes the same argument for stopping): a run started
// here survives this terminal closing, and the browser can start the same
// thing.
func runEvalRun(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("eval run", flag.ContinueOnError)
	suiteName := fs.String("suite", "", "a built-in suite's name, or a path to a suite JSON file (required)")
	variantList := fs.String("variants", "", "comma-separated prompt variants to compare, baseline first (required)")
	replicates := fs.Int("n", 3, "runs per task per variant")
	concurrency := fs.Int("concurrency", 2, "runs in flight at once")
	useJudge := fs.Bool("judge", false, "score each transcript with a model as well as the counters")
	judgeModel := fs.String("judge-model", "", "model the judge uses (config default otherwise)")
	maxSubTurns := fs.Int("max-sub-turns", 0, "override every task's sub-turn budget; applies to all arms at once")
	model := fs.String("model", "", "override the model every task runs on; applies to all arms at once")
	effort := fs.String("effort", "", "override the reasoning effort every task runs at; applies to all arms at once")
	note := fs.String("note", "", "one line on what this run is asking, shown beside it later")
	detach := fs.Bool("detach", false, "print the run id and exit rather than following it")
	out := fs.String("out", "", "write the finished report as JSON to this path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *suiteName == "" || *variantList == "" {
		return errors.New("usage: harness eval run -suite NAME|FILE -variants a,b [flags]")
	}

	spec := evals.Spec{
		Variants:    splitList(*variantList),
		Replicates:  *replicates,
		Concurrency: *concurrency,
		MaxSubTurns: *maxSubTurns,
		Model:       *model,
		Effort:      *effort,
		Judge:       *useJudge,
		JudgeModel:  *judgeModel,
		Note:        *note,
	}
	// A built-in name goes by name so the server resolves its own copy; a
	// path is read here and posted inline, which is how an ad-hoc suite runs
	// without a rebuild.
	if _, err := evals.EmbeddedSuite(*suiteName); err == nil {
		spec.Suite = *suiteName
	} else {
		suite, err := evals.LoadSuite(*suiteName)
		if err != nil {
			return err
		}
		spec.SuiteJSON = suite
	}
	suite, err := spec.Resolve()
	if err != nil {
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

	var started struct {
		EvalRunID string `json:"eval_run_id"`
	}
	if err := postJSON(ctx, cfg, st, "/api/evals", spec, http.StatusAccepted, &started); err != nil {
		return err
	}

	fmt.Printf("eval run %s: %d runs (%d tasks × %d variants × %d replicates)\n",
		started.EvalRunID, spec.TotalRuns(suite), len(suite.Tasks), len(spec.Variants), spec.Replicates)
	if *detach {
		fmt.Println("running in harness serve; follow it at /evals or with harness eval show")
		return nil
	}
	return followEval(ctx, st, started.EvalRunID, *out)
}

// followEval prints each member as it lands and the table at the end. It
// reads the stored rows rather than holding the run, so interrupting this
// leaves the eval going.
func followEval(ctx context.Context, st *store.Store, evalRunID, out string) error {
	seen := map[string]bool{}
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		run, err := st.GetEvalRun(ctx, evalRunID)
		if err != nil {
			return err
		}
		members, err := st.EvalMembers(ctx, evalRunID)
		if err != nil {
			return err
		}
		for _, m := range members {
			if m.Status == "pending" || seen[m.RequestID] {
				continue
			}
			seen[m.RequestID] = true
			status := m.Status
			if m.Error != "" {
				status = "error: " + m.Error
			}
			fmt.Printf("  %-24s %-20s rep %d  %s\n", m.TaskID, m.Variant, m.Replicate, status)
		}
		if run.FinishedAt != nil {
			report := evals.ReportFromStore(run, members)
			fmt.Println()
			evals.WriteTable(os.Stdout, report)
			if out != "" {
				if err := writeReport(out, report); err != nil {
					return err
				}
				fmt.Printf("\nreport written to %s\n", out)
			}
			return nil
		}
		select {
		case <-ctx.Done():
			fmt.Printf("\nstopped following; the run continues as %s\n", evalRunID)
			return nil
		case <-ticker.C:
		}
	}
}

// postJSON is the CLI's one call into the run-control surface: same origin,
// application/json, bearer token — the guards docs/RUN-CONTROL.md sets.
func postJSON(ctx context.Context, cfg config.Config, st *store.Store, path string, body any, wantStatus int, into any) error {
	token, err := settings.NewResolver(st).String(ctx, settings.KeyHTTPControlToken)
	if err != nil {
		return err
	}
	if token == "" {
		return errors.New("run control is not configured: http.control_token is empty (start harness serve once to generate one)")
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return err
	}
	url := "http://" + cfg.HTTPAddr + path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return fmt.Errorf("POST %s: %w", url, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != wantStatus {
		msg := strings.TrimSpace(string(respBody))
		if msg == "" {
			msg = resp.Status
		}
		return fmt.Errorf("POST %s: status %d: %s", path, resp.StatusCode, msg)
	}
	if into == nil {
		return nil
	}
	return json.Unmarshal(respBody, into)
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

func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}
