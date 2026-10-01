// Command shot takes a screenshot of a page with device emulation, to check
// the UI at phone / tablet / desktop widths (see .claude/skills/adesgo-ui).
//
//	go run . <url> <out.png> <width> <height>
//
// Widths below 1000px emulate a touch device (mobile viewport, 2x scale).
// Set CHROME_PATH if Chromium/Chrome isn't in the default macOS location.
// DARK=1 emulates a system dark theme (prefers-color-scheme: dark).
// EVAL="<js>" runs a script after the page loads (e.g. to scroll), before the shot.
package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/chromedp"
)

func main() {
	if len(os.Args) != 5 {
		fmt.Fprintln(os.Stderr, "usage: shot <url> <out.png> <width> <height>")
		os.Exit(2)
	}
	url, out := os.Args[1], os.Args[2]
	w, _ := strconv.Atoi(os.Args[3])
	h, _ := strconv.Atoi(os.Args[4])

	chrome := os.Getenv("CHROME_PATH")
	if chrome == "" {
		chrome = "/Applications/Chromium.app/Contents/MacOS/Chromium"
		if _, err := os.Stat(chrome); err != nil {
			chrome = "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
		}
	}
	opts := append(chromedp.DefaultExecAllocatorOptions[:], chromedp.ExecPath(chrome))
	actx, cancel := chromedp.NewExecAllocator(context.Background(), opts...)
	defer cancel()
	ctx, cancel := chromedp.NewContext(actx)
	defer cancel()
	ctx, cancel = context.WithTimeout(ctx, 40*time.Second)
	defer cancel()

	viewport := chromedp.EmulateViewport(int64(w), int64(h), chromedp.EmulateScale(1))
	if w < 1000 {
		viewport = chromedp.EmulateViewport(int64(w), int64(h), chromedp.EmulateScale(2), chromedp.EmulateMobile)
	}
	scheme := "light"
	if os.Getenv("DARK") == "1" {
		scheme = "dark"
	}
	media := emulation.SetEmulatedMedia().WithFeatures([]*emulation.MediaFeature{{Name: "prefers-color-scheme", Value: scheme}})
	actions := []chromedp.Action{viewport, media, chromedp.Navigate(url), chromedp.Sleep(800 * time.Millisecond)}
	if js := os.Getenv("EVAL"); js != "" {
		actions = append(actions, chromedp.Evaluate(js, nil), chromedp.Sleep(300*time.Millisecond))
	}
	var buf []byte
	actions = append(actions, chromedp.CaptureScreenshot(&buf))
	if err := chromedp.Run(ctx, actions...); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := os.WriteFile(out, buf, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
