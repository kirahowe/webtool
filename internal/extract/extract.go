// Package extract reads a page's text in a child process, so that no page
// can cost the fetcher more memory or time than it allows. The HTML parser
// has inputs whose cost grows far faster than their size, and it cannot be
// stopped once started; a process can. webtool runs itself with the hidden
// subcommand Command, sends the body on stdin and reads the Page back as
// strict JSON.
//
// What is bounded: the child's memory (it watches its own and exits with
// status 3 past Runner.Memory), its time (the parent kills it when the
// fetch's context ends), its reach (no environment, and on macOS no
// network and no file writes under sandbox-exec), and what the parent
// reads back (a Page with the agent's text cut at Runner.TextChars and a
// size cap of about four times the body). The parent's own memory is the
// body plus that Page.
package extract

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"strings"
	"time"

	"github.com/kirahowe/webtool/internal/text"
)

// Command is the hidden subcommand that runs a child.
const Command = "__extract"

// DefaultMemory is the child's memory limit when none is given.
const DefaultMemory = 512 << 20

// The errors a page can give the agent. ErrTooCostly means the child
// passed its memory limit, ErrTimedOut that it was killed at the deadline;
// any other failure is ErrUnreadable, with the child's reason on stderr.
var (
	ErrTooCostly  = errors.New("the page was too costly to read")
	ErrTimedOut   = errors.New("timed out reading the page's text")
	ErrUnreadable = errors.New("could not read the page")
)

// request is the header line the parent sends before the body.
type request struct {
	MediaType string `json:"media_type"`
	Base      string `json:"base"`
	Limit     int    `json:"limit"`
	TextChars int    `json:"text_chars"`
	Memory    int64  `json:"memory"`
	BodyBytes int    `json:"body_bytes"` // exactly this many bytes follow
}

const (
	maxHeaderBytes = 64 << 10
	maxBodyBytes   = 256 << 20
	exitMemory     = 3
)

// Child runs in the child process: it reads one request and body from
// stdin and writes the Page to stdout. Nothing else is written there.
func Child(stdin io.Reader, stdout io.Writer) int {
	r := bufio.NewReaderSize(io.LimitReader(stdin, maxHeaderBytes+maxBodyBytes+1), maxHeaderBytes)
	line, err := r.ReadSlice('\n')
	if err != nil {
		return 2
	}
	var req request
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || req.Limit <= 0 || req.Memory <= 0 || req.TextChars <= 0 ||
		req.BodyBytes < 0 || req.BodyBytes > maxBodyBytes {
		fmt.Fprintln(os.Stderr, "webtool reader: bad request")
		return 2
	}
	debug.SetMemoryLimit(req.Memory)
	go watch(req.Memory)
	// The body is exactly as long as the header says: a short read, or
	// anything after it, is a failure rather than a page read in part.
	body := make([]byte, req.BodyBytes)
	if _, err := io.ReadFull(r, body); err != nil {
		fmt.Fprintln(os.Stderr, "webtool reader: the body is shorter than its header says")
		return 2
	}
	if n, _ := r.Read(make([]byte, 1)); n != 0 {
		fmt.Fprintln(os.Stderr, "webtool reader: the body is longer than its header says")
		return 2
	}
	var base *url.URL
	if req.Base != "" {
		if base, err = url.Parse(req.Base); err != nil {
			return 2
		}
	}
	page := text.Read(context.Background(), body, req.MediaType, base, req.Limit, req.TextChars)
	enc := json.NewEncoder(stdout)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(page); err != nil {
		return 2
	}
	return 0
}

// watch exits the child once it holds more than limit bytes. A page can
// make the parser allocate faster than the garbage collector frees, and
// the soft limit in debug.SetMemoryLimit only makes it try harder.
func watch(limit int64) {
	s := []metrics.Sample{{Name: "/memory/classes/total:bytes"}}
	for {
		metrics.Read(s)
		if s[0].Value.Kind() == metrics.KindUint64 && int64(s[0].Value.Uint64()) > limit {
			os.Exit(exitMemory)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// Runner starts children.
type Runner struct {
	// Argv starts a child: the webtool binary and Command.
	Argv      []string
	Memory    int64 // the child's memory limit in bytes
	TextChars int   // the agent's text is cut here (max_text_chars)
	BodyBytes int   // the largest body read (max_body_bytes)
	// Sandbox runs the child under sandbox-exec, with no network and no
	// file writes. webtool requires it on macOS unless configured off.
	Sandbox bool
	Stderr  io.Writer // gets the end of a failed child's stderr
}

// Self is the Runner for this binary.
func Self(r Runner) (Runner, error) {
	exe, err := os.Executable()
	if err != nil {
		return Runner{}, fmt.Errorf("cannot find webtool's own binary to read pages in: %w", err)
	}
	r.Argv = []string{exe, Command}
	if r.Memory <= 0 {
		r.Memory = DefaultMemory
	}
	return r, nil
}

// SandboxPath is the macOS sandbox tool.
const SandboxPath = "/usr/bin/sandbox-exec"

// sandboxProfile denies the child the network and any file writes. The
// child needs neither: it reads stdin and writes stdout.
const sandboxProfile = `(version 1)(allow default)(deny network*)(deny file-write*)`

// SandboxAvailable reports whether this system can run the child in the
// sandbox: macOS with a sandbox-exec that applies the profile.
func SandboxAvailable() error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("the reader sandbox needs macOS's sandbox-exec, and this is %s", runtime.GOOS)
	}
	if err := exec.Command(SandboxPath, "-p", sandboxProfile, "/usr/bin/true").Run(); err != nil {
		return fmt.Errorf("%s cannot apply the reader's profile: %v", SandboxPath, err)
	}
	return nil
}

// Command is the child's full argv, sandbox included.
func (r Runner) Command() []string {
	if r.Sandbox {
		return append([]string{SandboxPath, "-p", sandboxProfile}, r.Argv...)
	}
	return r.Argv
}

// maxBaseBytes bounds the page's URL sent to the child.
const maxBaseBytes = 32 << 10

// Extract reads a body in a child and returns its Page. The child is
// killed when ctx is done. Any failure is one of the errors above, never a
// partial Page.
//
// What the parent holds is bounded: the Page it reads back has its agent
// text cut at TextChars, and its visible text (needed whole to check
// quotes) stripped of the characters quote matching ignores, so it is no
// more than about three times the body (see readBackCap, which allows
// four). Output past the cap is refused, not read.
func (r Runner) Extract(ctx context.Context, body []byte, mediaType string, base *url.URL, limit int) (text.Page, error) {
	b := ""
	if base != nil {
		b = base.String()
	}
	if len(b) > maxBaseBytes {
		return text.Page{}, fmt.Errorf("the page's URL is longer than %d bytes", maxBaseBytes)
	}
	textChars := r.TextChars
	if textChars <= 0 {
		textChars = limit
	}
	if len(body) > maxBodyBytes {
		return text.Page{}, fmt.Errorf("the body is longer than %d bytes", maxBodyBytes)
	}
	header, err := json.Marshal(request{MediaType: mediaType, Base: b, Limit: limit, TextChars: textChars, Memory: r.Memory, BodyBytes: len(body)})
	if err != nil {
		return text.Page{}, err
	}
	argv := r.Command()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = []string{} // nothing from the parent's environment
	cmd.Stdin = io.MultiReader(bytes.NewReader(header), bytes.NewReader([]byte("\n")), bytes.NewReader(body))
	bodyBytes := max(r.BodyBytes, len(body))
	limitOut := readBackCap(bodyBytes, textChars)
	errTail := &tail{max: 4 << 10}
	cmd.Stderr = errTail
	cmd.WaitDelay = time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return text.Page{}, err
	}
	if err := cmd.Start(); err != nil {
		r.report(err.Error(), errTail)
		return text.Page{}, ErrUnreadable
	}
	// The Page is decoded straight from the pipe, with no copy of the
	// output beside the decoder's own, and reading stops at the cap.
	out := &io.LimitedReader{R: stdout, N: limitOut + 1}
	var page text.Page
	dec := json.NewDecoder(out)
	dec.DisallowUnknownFields()
	decErr := dec.Decode(&page)
	if decErr == nil {
		if _, err := dec.Token(); err != io.EOF {
			decErr = errors.New("more than one page")
		}
	}
	if decErr != nil {
		cmd.Process.Kill()
	}
	io.Copy(io.Discard, out)
	over := out.N <= 0
	runErr := cmd.Wait()
	var exit *exec.ExitError
	switch {
	case ctx.Err() != nil:
		return text.Page{}, ErrTimedOut
	case errors.As(runErr, &exit) && exit.ExitCode() == exitMemory:
		return text.Page{}, ErrTooCostly
	case over:
		r.report("its output was larger than any page's", errTail)
		return text.Page{}, ErrUnreadable
	case runErr != nil || decErr != nil:
		var why []string
		if decErr != nil {
			why = append(why, "its output was not one page: "+decErr.Error())
		}
		if runErr != nil {
			why = append(why, "it ended with "+runErr.Error())
		}
		r.report(strings.Join(why, "; "), errTail)
		return text.Page{}, ErrUnreadable
	}
	return page, nil
}

// readBackCap bounds the JSON a child may write for a body of bodyBytes.
// The visible text is at most three times the body: each byte of invalid
// UTF-8 becomes U+FFFD, three bytes, and JSON at most doubles anything
// else (a quote, a backslash, a newline). The agent's text and the title
// are cut, and JSON writes no rune in more than six bytes. The cap allows
// four times the body and sixteen bytes a rune, so a third more than the
// worst page the reader can write, and the fields and their names fit in
// the 64 KiB beside.
func readBackCap(bodyBytes, textChars int) int64 {
	return int64(4*bodyBytes + 16*textChars + 16*text.MaxTitleRunes + 1<<16)
}

// report writes why a child failed, and the end of its stderr, to the
// operator's log.
func (r Runner) report(why string, t *tail) {
	if r.Stderr == nil {
		return
	}
	fmt.Fprintf(r.Stderr, "webtool: the page reader failed: %s\n", why)
	if t.b.Len() > 0 {
		fmt.Fprintf(r.Stderr, "webtool: the reader's stderr ended:\n%s\n", text.EscapeOnly(t.b.String()))
	}
}

// tail keeps the last max bytes written.
type tail struct {
	b   bytes.Buffer
	max int
}

func (t *tail) Write(p []byte) (int, error) {
	t.b.Write(p)
	if over := t.b.Len() - t.max; over > 0 {
		rest := append([]byte(nil), t.b.Bytes()[over:]...)
		t.b.Reset()
		t.b.Write(rest)
	}
	return len(p), nil
}
