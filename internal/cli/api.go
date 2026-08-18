package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/9technologygroup/docker-updater/internal/config"
	"github.com/9technologygroup/docker-updater/internal/job"
)

const maxAPIBody = 4 << 20

type apiClient struct {
	base  string
	token string
	http  *http.Client
}

func newAPIClient(cfg *config.Config) (*apiClient, error) {
	token := string(cfg.BearerToken())
	if token == "" {
		return nil, fmt.Errorf("no bearer token is configured, so this command cannot talk to the API")
	}
	return &apiClient{
		base:  "http://" + cfg.Listen,
		token: token,
		http:  &http.Client{Timeout: 6 * time.Minute},
	}, nil
}

func (c *apiClient) do(ctx context.Context, method, path, body string, out any) error {
	var reader io.Reader
	if body != "" {
		reader = bytes.NewReader([]byte(body))
	}

	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return apiUnreachable(c.base, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxAPIBody))
	if resp.StatusCode >= 400 && resp.StatusCode != http.StatusInternalServerError {
		return fmt.Errorf("the API refused the request (%d): %s", resp.StatusCode, apiError(raw))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("could not read the API response: %w", err)
	}
	return nil
}

func apiError(raw []byte) string {
	var parsed struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &parsed); err == nil && parsed.Error != "" {
		return parsed.Error
	}
	return strings.TrimSpace(string(raw))
}

func runUpdate(args []string) error {
	target, args := popPositional(args)

	fs, configPath := newFlagSet("update")
	tag := fs.String("tag", "", "image tag to deploy (only for targets with image_tag_env set)")
	reason := fs.String("reason", "", "why this update is happening, recorded on the job")
	dryRun := fs.Bool("dry-run", false, "report what would happen without touching anything")
	force := fs.Bool("force", false, "recreate even if the images have not changed")
	wait := fs.Duration("wait", 4*time.Minute, "how long to wait for the update to finish")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if target == "" && fs.NArg() == 1 {
		target = fs.Arg(0)
	}
	if target == "" || fs.NArg() > 1 {
		return fmt.Errorf("usage: dup update <stack> [flags]")
	}

	cfg, err := config.LoadService(*configPath)
	if err != nil {
		return err
	}
	if _, ok := cfg.Target(target); !ok {
		return fmt.Errorf("unknown stack %q; run 'dup list' to see what is configured", target)
	}

	client, err := newAPIClient(cfg)
	if err != nil {
		return err
	}

	body, err := json.Marshal(map[string]any{
		"tag": *tag, "reason": *reason, "dry_run": *dryRun, "force": *force,
	})
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), *wait+time.Minute)
	defer cancel()

	var snap job.Snapshot
	path := fmt.Sprintf("/v1/targets/%s/update?wait=%s", target, wait.String())
	if err := client.do(ctx, http.MethodPost, path, string(body), &snap); err != nil {
		return err
	}

	printJob(snap)
	if !snap.State.OK() {
		return fmt.Errorf("update finished as %s", snap.State)
	}
	return nil
}

func printJob(snap job.Snapshot) {
	fmt.Printf("%s  %s\n", snap.Target, snap.State)
	if snap.Message != "" {
		fmt.Printf("  %s\n", snap.Message)
	}
	if len(snap.Changed) > 0 {
		fmt.Printf("  changed: %s\n", strings.Join(snap.Changed, ", "))
	}
	fmt.Printf("  job %s in %s\n", snap.ID, time.Duration(snap.DurationMS)*time.Millisecond)

	for _, step := range snap.Steps {
		mark := "ok"
		if !step.OK {
			mark = "FAILED"
		}
		fmt.Printf("    %-24s %s\n", step.Name, mark)
		if step.Error != "" {
			fmt.Printf("      %s\n", step.Error)
		}
	}
}

func runStatus(args []string) error {
	target, args := popPositional(args)

	fs, configPath := newFlagSet("status")
	limit := fs.Int("limit", 10, "how many jobs to show")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.LoadService(*configPath)
	if err != nil {
		return err
	}
	client, err := newAPIClient(cfg)
	if err != nil {
		return err
	}

	if target == "" && fs.NArg() == 1 {
		target = fs.Arg(0)
	}
	path := fmt.Sprintf("/v1/jobs?limit=%d", *limit)
	if target != "" {
		if _, ok := cfg.Target(target); !ok {
			return fmt.Errorf("unknown stack %q; run 'dup list' to see what is configured", target)
		}
		path += "&target=" + target
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var out struct {
		Jobs []job.Snapshot `json:"jobs"`
	}
	if err := client.do(ctx, http.MethodGet, path, "", &out); err != nil {
		return err
	}
	if len(out.Jobs) == 0 {
		fmt.Println("no updates recorded yet")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "WHEN\tSTACK\tSTATE\tTRIGGER\tTOOK\tDETAIL")
	for _, j := range out.Jobs {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			j.StartedAt.Local().Format("02 Jan 15:04"), j.Target, j.State,
			orDash(j.Trigger), (time.Duration(j.DurationMS) * time.Millisecond).Round(time.Second), j.Message)
	}
	return w.Flush()
}
