package cmd

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"math"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/vbauerster/mpb/v8"
	"github.com/vbauerster/mpb/v8/decor"
)

var (
	depth       int
	concurrency int
)

var transcodeCmd = &cobra.Command{
	Use:   "transcode",
	Short: "Transcode large mp4 files to h.265",
	Long:  `Recursively find mp4 files (>100MB) and transcode them to h.265 if they are not already.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		// Setup context with cancellation on signal
		ctx, cancel := context.WithCancel(cmd.Context())
		defer cancel()

		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
		go func() {
			<-sigCh
			cancel()
		}()

		targets := args
		if len(targets) == 0 {
			wd, err := os.Getwd()
			if err != nil {
				return fmt.Errorf("failed to get current working directory: %w", err)
			}
			targets = []string{wd}
		}

		plan, err := planTranscode(ctx, targets)
		if err != nil {
			return err
		}

		return runTranscode(ctx, plan)
	},
}

func init() {
	transcodeCmd.Flags().IntVarP(&depth, "depth", "d", 5, "maximum depth to traverse")
	transcodeCmd.Flags().IntVarP(&concurrency, "concurrency", "j", 1, "maximum number of concurrent transcoding jobs")
}

func isH265(path string) (bool, error) {
	cmd := exec.Command("ffprobe", "-v", "error", "-select_streams", "v:0", "-show_entries", "stream=codec_name", "-of", "default=noprint_wrappers=1:nokey=1", path)
	var out bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return false, fmt.Errorf("ffprobe failed: %w (stderr: %s)", err, stderr.String())
	}
	return strings.TrimSpace(out.String()) == "hevc", nil
}

type transcodeEvent struct {
	done     bool
	progress float64
	total    float64
	err      error
}

func transcodeFile(ctx context.Context, inputPath string) (<-chan transcodeEvent, error) {
	// Initial checks and setup
	_, err := os.Stat(inputPath)
	if err != nil {
		return nil, fmt.Errorf("failed to stat input file: %w", err)
	}

	ext := filepath.Ext(inputPath)
	base := inputPath[:len(inputPath)-len(ext)]
	outputPath := base + ".h265.mp4"

	// Get input duration for verification and progress calculation
	inputDuration, err := getDuration(inputPath)
	if err != nil {
		return nil, fmt.Errorf("failed to get input duration: %w", err)
	}

	ch := make(chan transcodeEvent)

	go func() {
		defer close(ch)

		// Create command
		// ffmpeg -i "$INPUT_FILE" \
		//   -c:v hevc_videotoolbox \
		//   -q:v 65 \
		//   -tag:v hvc1 \
		//   -c:a aac -b:a 192k \
		//   "$OUTPUT_FILE"
		cmd := exec.CommandContext(ctx, "ffmpeg", "-i", inputPath,
			"-c:v", "hevc_videotoolbox",
			"-q:v", "65",
			"-tag:v", "hvc1",
			"-c:a", "aac", "-b:a", "192k",
			"-progress", "pipe:2",
			"-nostats",
			"-y",
			outputPath)

		stderr, err := cmd.StderrPipe()
		if err != nil {
			ch <- transcodeEvent{err: fmt.Errorf("failed to get stderr pipe: %w", err)}
			return
		}

		if err := cmd.Start(); err != nil {
			ch <- transcodeEvent{err: fmt.Errorf("failed to start ffmpeg: %w", err)}
			return
		}

		// Ensure cleanup
		var success bool
		defer func() {
			if !success {
				os.Remove(outputPath)
			}
		}()

		// Progress parsing
		scanner := bufio.NewScanner(stderr)
		scanner.Split(bufio.ScanLines)
		timeRegex := regexp.MustCompile(`out_time=(\d+):(\d+):(\d+(?:\.\d+)?)`)

		for scanner.Scan() {
			line := scanner.Text()
			if matches := timeRegex.FindStringSubmatch(line); matches != nil {
				current := parseDurationComponents(matches[1], matches[2], matches[3])
				progress := current.Seconds() / inputDuration
				if progress > 1.0 {
					progress = 1.0
				}

				// Send progress
				// Non-blocking send isn't strictly necessary with a dedicated goroutine per file
				// but user wants progress channel communication.
				// We should just block as the consumer is likely reading from it.
				// However, if consumer stops reading, we might block.
				// But we are in a goroutine that is dedicated to this task.
				// If we want to support cancellation, we should select on ctx.Done() too.
				select {
				case ch <- transcodeEvent{progress: progress, total: 1.0}:
				case <-ctx.Done():
					// Context canceled, process likely killed by cmd.Wait() or will be.
					return
				}
			}
		}

		if err := cmd.Wait(); err != nil {
			ch <- transcodeEvent{err: fmt.Errorf("ffmpeg execution failed: %w", err)}
			return
		}

		// Output verification moved to finalizeFile
		// We do NOT replace files here anymore.
		// That is the caller's responsibility.
		// Use strictly only for verification logic that determines "success" of transcoding.

		success = true
		ch <- transcodeEvent{done: true, progress: 1.0, total: 1.0}
	}()

	return ch, nil
}

func getDuration(path string) (float64, error) {
	cmd := exec.Command("ffprobe", "-v", "error", "-show_entries", "format=duration", "-of", "default=noprint_wrappers=1:nokey=1", path)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("%w: %s", err, stderr.String())
	}
	val := strings.TrimSpace(string(out))
	return strconv.ParseFloat(val, 64)
}

func parseDurationComponents(h, m, s string) time.Duration {
	hours, _ := strconv.Atoi(h)
	minutes, _ := strconv.Atoi(m)
	seconds, _ := strconv.ParseFloat(s, 64)
	return time.Duration(hours)*time.Hour +
		time.Duration(minutes)*time.Minute +
		time.Duration(seconds*float64(time.Second))
}

type transcodePlan struct {
	queue []string
	total int
	lock  *sync.Mutex
}

func (p *transcodePlan) dequeue() (string, bool) {
	p.lock.Lock()
	defer p.lock.Unlock()

	if len(p.queue) == 0 {
		return "", false
	}

	item := p.queue[0]
	p.queue = p.queue[1:]
	return item, true
}

func planTranscode(ctx context.Context, targets []string) (*transcodePlan, error) {
	var queue []string
	for _, target := range targets {
		info, err := os.Stat(target)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error accessing %s: %v\n", target, err)
			continue
		}

		if !info.IsDir() {
			// Process single file
			if shouldProcess(target, info) {
				queue = append(queue, target)
			}
			continue
		}

		// It's a directory, walk it
		root, err := filepath.Abs(target)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error resolving path %s: %v\n", target, err)
			continue
		}

		err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}

			if ctx.Err() != nil {
				return ctx.Err()
			}

			// Calculate depth relative to the target root
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}

			if rel == "." {
				return nil
			}

			depthCount := strings.Count(rel, string(os.PathSeparator)) + 1
			if d.IsDir() {
				if depthCount > depth {
					return filepath.SkipDir
				}
				return nil
			}

			info, err := d.Info()
			if err != nil {
				return err
			}

			if shouldProcess(path, info) {
				queue = append(queue, path)
			}
			return nil
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error walking directory %s: %v\n", target, err)
		}
	}
	return &transcodePlan{
		queue: queue,
		total: len(queue),
		lock:  &sync.Mutex{},
	}, nil
}

func shouldProcess(path string, info os.FileInfo) bool {
	if strings.ToLower(filepath.Ext(path)) != ".mp4" {
		return false
	}
	// Skip files that look like our own output artifacts
	if strings.HasSuffix(path, ".h265.mp4") {
		return false
	}
	if info.Size() < 100*1024*1024 {
		return false
	}
	isHevc, err := isH265(path)
	if err != nil {
		fmt.Printf("Skipping %s: error checking codec: %v\n", path, err)
		return false
	}
	if isHevc {
		fmt.Printf("Skipping %s: already h.265\n", path)
		return false
	}
	// It is a valid candidate
	// fmt.Printf("Queuing %s\n", path) // Optional: confirm queuing
	return true
}

type ErrorTracker struct {
	mu     sync.Mutex
	errors []errorInfo
}

type errorInfo struct {
	file string
	err  error
}

func (e *ErrorTracker) Add(file string, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.errors = append(e.errors, errorInfo{file: file, err: err})
}

func (e *ErrorTracker) PrintSummary() {
	if len(e.errors) == 0 {
		return
	}
	fmt.Printf("\nCompleted with %d errors:\n", len(e.errors))
	for _, info := range e.errors {
		fmt.Printf("- %s: %v\n", info.file, info.err)
	}
}

func runTranscode(ctx context.Context, plan *transcodePlan) error {
	if plan.total == 0 {
		fmt.Println("No files to transcode.")
		return nil
	}

	p := mpb.New(mpb.WithWidth(64))
	tracker := &ErrorTracker{}

	// Global progress bar
	globalBar := p.AddBar(int64(plan.total),
		mpb.PrependDecorators(
			decor.Meta(
				decor.Name("Total", decor.WC{C: decor.DindentRight, W: 1}, decor.WCSyncWidth),
				func(s string) string { return "\033[32m" + s + "\033[0m" },
			),
			decor.Percentage(decor.WCSyncSpace),
		),
		mpb.AppendDecorators(
			decor.CountersNoUnit("%d / %d", decor.WCSyncSpace),
		),
	)

	var wg sync.WaitGroup

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				target, ok := plan.dequeue()
				if !ok {
					return
				}

				if ctx.Err() != nil {
					return
				}

				processFile(ctx, target, p, tracker)
				globalBar.Increment()
			}
		}()
	}

	// Monitor context cancellation to abort global bar
	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case <-ctx.Done():
			globalBar.Abort(true)
		case <-done:
		}
	}()

	wg.Wait()
	if ctx.Err() != nil {
		globalBar.Abort(true)
	}
	p.Wait()
	<-done // wait for monitor to exit checks
	tracker.PrintSummary()
	return nil
}

func processFile(ctx context.Context, target string, p *mpb.Progress, tracker *ErrorTracker) {
	baseName := filepath.Base(target)
	bar := p.AddBar(1000,
		mpb.BarRemoveOnComplete(),
		mpb.PrependDecorators(
			decor.Name(baseName, decor.WC{C: decor.DindentRight, W: 1}, decor.WCSyncWidth),
			decor.Percentage(decor.WCSyncSpace),
		),
		mpb.AppendDecorators(
			decor.OnComplete(
				decor.Elapsed(decor.ET_STYLE_GO, decor.WC{W: 5}), "Done",
			),
		),
	)

	ch, err := transcodeFile(ctx, target)
	if err != nil {
		tracker.Add(target, fmt.Errorf("failed to initiate transcode: %w", err))
		bar.Abort(true)
		return
	}

	var completed bool
	for evt := range ch {
		if evt.err != nil {
			tracker.Add(target, evt.err)
			bar.Abort(true)
			return
		}
		if evt.done {
			bar.SetCurrent(1000)
			bar.SetTotal(1000, true)
			if err := finalizeFile(target); err != nil {
				tracker.Add(target, fmt.Errorf("finalization failed: %w", err))
				bar.Abort(true)
				return
			}
			completed = true
		} else {
			val := int64(evt.progress * 1000)
			bar.SetCurrent(val)
		}
	}

	if !completed {
		bar.Abort(true)
	}
}

func finalizeFile(target string) error {
	info, err := os.Stat(target)
	if err != nil {
		return fmt.Errorf("error stating file %s: %w", target, err)
	}

	ext := filepath.Ext(target)
	base := target[:len(target)-len(ext)]
	outputPath := base + ".h265.mp4"

	// Verify Duration Logic
	inputDuration, err := getDuration(target)
	if err != nil {
		return fmt.Errorf("failed to get input duration: %w", err)
	}

	outputDuration, err := getDuration(outputPath)
	if err != nil {
		return fmt.Errorf("failed to get output duration: %w", err)
	}

	diff := math.Abs(inputDuration - outputDuration)
	if diff > 10.0 {
		return fmt.Errorf("duration mismatch > 10s: input=%.2fs, output=%.2fs", inputDuration, outputDuration)
	}

	// Restore modification time
	if err := os.Chtimes(outputPath, info.ModTime(), info.ModTime()); err != nil {
		// Just return error or ignore?
		// Since we want to suppress output, we can swallow it or wrap it?
		// User didn't ask to swallow errors, but we can't print.
		// Let's just ignore the warning here as it's non-critical, or maybe return it?
		// Original code just printed a warning.
		// We'll leave it be for now but NOT print.
	}

	// Remove input file
	if err := os.Remove(target); err != nil {
		return fmt.Errorf("failed to remove input file %s: %w", target, err)
	}

	// Rename output to input
	if err := os.Rename(outputPath, target); err != nil {
		return fmt.Errorf("failed to rename output file %s: %w", outputPath, err)
	}
	// fmt.Printf("Done: %s\n", target) // Removed to prevent mpb interference
	return nil
}
