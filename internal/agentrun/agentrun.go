// Package agentrun is the placeholder "computer use" agent: boot-check
// Chromium, run a web search for the job's prompt, and record the session as
// a series of screenshots (the flight recorder).
package agentrun

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"sync"
	"time"

	"github.com/chromedp/chromedp"
)

// Save persists one artifact (screenshot) somewhere: S3 in Fargate, disk in
// local mode.
type Save func(ctx context.Context, name string, data []byte) error

type Result struct {
	Screenshots int
	FinalURL    string
}

// Execute runs the browser task under ctx (the caller sets the TTL deadline —
// reaper layer 1). bootTimeout bounds Chromium startup: if the environment is
// hung before the agent even starts, we fail fast with a distinct error.
func Execute(ctx context.Context, prompt string, bootTimeout time.Duration, save Save) (*Result, error) {
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.NoSandbox,
		chromedp.Flag("disable-dev-shm-usage", true),
		chromedp.WindowSize(1280, 800),
	)
	if p := os.Getenv("CHROME_PATH"); p != "" {
		opts = append(opts, chromedp.ExecPath(p))
	}
	allocCtx, cancelAlloc := chromedp.NewExecAllocator(ctx, opts...)
	defer cancelAlloc()
	browserCtx, cancelBrowser := chromedp.NewContext(allocCtx)
	defer cancelBrowser()

	// chromedp contexts are not safe for concurrent Run; one mutex serializes
	// the task steps and the flight-recorder goroutine.
	var mu sync.Mutex
	run := func(actions ...chromedp.Action) error {
		mu.Lock()
		defer mu.Unlock()
		return chromedp.Run(browserCtx, actions...)
	}

	// Boot health check: a hung/absent Chromium fails here, before the task.
	// The first Run starts the browser, and chromedp ties the browser's
	// lifetime to the context of that Run — so the timeout must NOT wrap the
	// context, or the browser dies when the timeout fires. Bound it externally.
	bootErr := make(chan error, 1)
	go func() { bootErr <- chromedp.Run(browserCtx, chromedp.Navigate("about:blank")) }()
	select {
	case err := <-bootErr:
		if err != nil {
			return nil, fmt.Errorf("env_unhealthy: browser failed to start: %w", err)
		}
	case <-time.After(bootTimeout):
		return nil, fmt.Errorf("env_unhealthy: browser failed to start within %s", bootTimeout)
	}
	slog.Info("browser booted")

	res := &Result{}
	snap := func(label string) {
		var buf []byte
		if err := run(chromedp.CaptureScreenshot(&buf)); err != nil {
			slog.Error("screenshot failed", "err", err, "label", label)
			return
		}
		name := fmt.Sprintf("screens/%d-%s.png", time.Now().UnixMilli(), label)
		if err := save(ctx, name, buf); err != nil {
			slog.Error("artifact save failed", "err", err, "name", name)
			return
		}
		res.Screenshots++
		slog.Info("screenshot saved", "name", name)
	}

	// Flight recorder: periodic screenshots uploaded as we go, so a crash
	// mid-run still leaves a replayable trail.
	recorderDone := make(chan struct{})
	recorderCtx, stopRecorder := context.WithCancel(ctx)
	go func() {
		defer close(recorderDone)
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-recorderCtx.Done():
				return
			case <-ticker.C:
				snap("recorder")
			}
		}
	}()
	defer func() { stopRecorder(); <-recorderDone }()

	// The task: search Wikipedia (no bot wall, stable selectors) and let the
	// page render. Steps are granular so the recorder can interleave.
	searchURL := "https://en.wikipedia.org/w/index.php?search=" + url.QueryEscape(prompt)
	slog.Info("task start", "url", searchURL)
	if err := run(chromedp.Navigate(searchURL)); err != nil {
		return res, fmt.Errorf("navigate failed: %w", err)
	}
	snap("loaded")
	if err := run(chromedp.WaitVisible(`#content`, chromedp.ByID)); err != nil {
		return res, fmt.Errorf("results never appeared: %w", err)
	}
	if err := run(chromedp.Sleep(2 * time.Second)); err != nil { // let images/fonts settle
		return res, err
	}
	snap("results")
	if err := run(chromedp.Location(&res.FinalURL)); err != nil {
		return res, err
	}
	slog.Info("task complete", "final_url", res.FinalURL, "screenshots", res.Screenshots)
	return res, nil
}
