package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

type PullRequest struct {
	Ref  string
	URL  string
	Base string
	Head string
}

type GitIdentity struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

// GitIdentityProvider is the narrow resource-plane seam used by workflow
// commits. Author identity is sealed in the C server's Vault and deliberately
// absent from the long-lived Go process environment, so production resolves it
// just in time over the local root-owned Unix socket.
type GitIdentityProvider interface {
	Identity(context.Context, string) (GitIdentity, error)
}

// PullRequestSpec is the review contract the workflow hands to the forge. A
// root workflow always sets Draft: automation may assemble and publish the
// handoff, but making it reviewable and merging it are human actions. Slice PRs
// stay non-draft because their CI-gated merge into the private feature branch
// is part of autonomous assembly.
type PullRequestSpec struct {
	Title string
	Body  string
	Draft bool
}
type CIState string

const (
	CIPending CIState = "pending"
	CIPassed  CIState = "passed"
	CIFailed  CIState = "failed"
)

type Forge interface {
	Push(context.Context, string, string, string) error
	Open(context.Context, string, string, string, string, PullRequestSpec) (PullRequest, error)
	CI(context.Context, string, string) (CIState, error)
	Merge(context.Context, string, string, string) error
}

type expectedOpenForge interface {
	OpenExpected(context.Context, string, string, string, string, string, PullRequestSpec) (PullRequest, error)
}

var (
	ErrForgePublication      = errors.New("forge publication failed")
	ErrForgeLeaseMismatch    = errors.New("forge lease mismatch")
	ErrForgeNonFastForward   = errors.New("forge non-fast-forward")
	ErrForgeUnverifiedCommit = errors.New("forge unverified commit")
)

type ForgePublicationError struct {
	Code   string
	Detail string
}

func (e *ForgePublicationError) Error() string { return e.Code + ": " + e.Detail }
func (e *ForgePublicationError) Is(target error) bool {
	if target == ErrForgePublication {
		return true
	}
	switch e.Code {
	case "lease_mismatch":
		return target == ErrForgeLeaseMismatch
	case "non_fast_forward":
		return target == ErrForgeNonFastForward
	case "unverified_commit":
		return target == ErrForgeUnverifiedCommit
	default:
		return false
	}
}
func (e *ForgePublicationError) Unwrap() error { return ErrForgePublication }

type unavailableForge struct{}

func (unavailableForge) Push(context.Context, string, string, string) error {
	return errors.New("forge resource plane is unavailable")
}
func (unavailableForge) Open(context.Context, string, string, string, string, PullRequestSpec) (PullRequest, error) {
	return PullRequest{}, errors.New("forge resource plane is unavailable")
}

func (f *HTTPForge) Push(ctx context.Context, repo, workdir, head string) error {
	return f.push(ctx, repo, workdir, head, "")
}

func (f *HTTPForge) PushExpected(ctx context.Context, repo, workdir, head, expectedRemote string) error {
	return f.push(ctx, repo, workdir, head, expectedRemote)
}

func (f *HTTPForge) push(ctx context.Context, repo, workdir, head, expectedRemote string) error {
	if !managedBranch(head) {
		return fmt.Errorf("refuse push of unmanaged branch %q", head)
	}
	repoOrigin, err := gitText(ctx, repo, "remote", "get-url", "origin")
	if err != nil {
		return err
	}
	workOrigin, err := gitText(ctx, workdir, "remote", "get-url", "origin")
	if err != nil || repoOrigin != workOrigin {
		return errors.New("worktree origin does not match admitted repository")
	}
	var ignored map[string]any
	input := map[string]any{"op": "push", "workdir": workdir, "head": head}
	if expectedRemote != "" {
		input["expected_remote"] = expectedRemote
	}
	return f.execute(ctx, input, &ignored)
}

func (f *HTTPForge) Identity(ctx context.Context, workdir string) (GitIdentity, error) {
	var result struct {
		Configured bool   `json:"configured"`
		Name       string `json:"name"`
		Email      string `json:"email"`
	}
	if err := f.execute(ctx, map[string]any{"op": "identity", "workdir": workdir}, &result); err != nil {
		return GitIdentity{}, err
	}
	if !result.Configured || strings.TrimSpace(result.Name) == "" || strings.TrimSpace(result.Email) == "" {
		return GitIdentity{}, ErrGitIdentityMissing
	}
	return GitIdentity{Name: result.Name, Email: result.Email}, nil
}
func (unavailableForge) CI(context.Context, string, string) (CIState, error) {
	return CIPending, errors.New("forge resource plane is unavailable")
}
func (unavailableForge) Merge(context.Context, string, string, string) error {
	return errors.New("forge resource plane is unavailable")
}

// HTTPForge asks the C resource plane to mechanically execute narrowly typed
// authenticated operations. Go remains the sole owner of workflow policy,
// timing, state, and transitions; credential material never crosses the socket.
type HTTPForge struct {
	baseURL, bearer string
	client          *http.Client
}

type HTTPForgeConfig struct {
	BaseURL, UnixSocket, Bearer string
	Timeout                     time.Duration
}

func NewHTTPForge(cfg HTTPForgeConfig) (*HTTPForge, error) {
	if cfg.BaseURL == "" {
		cfg.BaseURL = "http://aimee"
	}
	if cfg.UnixSocket == "" {
		return nil, errors.New("forge resource plane requires a Unix socket")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 2 * time.Minute
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	socket := cfg.UnixSocket
	transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}
	return &HTTPForge{baseURL: strings.TrimRight(cfg.BaseURL, "/"), bearer: cfg.Bearer,
		client: &http.Client{Transport: transport, Timeout: cfg.Timeout}}, nil
}

func (f *HTTPForge) Open(ctx context.Context, repo, workdir, head, base string, spec PullRequestSpec) (PullRequest, error) {
	return f.open(ctx, repo, workdir, head, base, "", spec)
}

func (f *HTTPForge) OpenExpected(ctx context.Context, repo, workdir, head, base, expectedRemote string, spec PullRequestSpec) (PullRequest, error) {
	return f.open(ctx, repo, workdir, head, base, expectedRemote, spec)
}

func (f *HTTPForge) open(ctx context.Context, repo, workdir, head, base, expectedRemote string, spec PullRequestSpec) (PullRequest, error) {
	if !managedBranch(head) {
		return PullRequest{}, fmt.Errorf("refuse push of unmanaged branch %q", head)
	}
	if base == "" || strings.HasPrefix(base, "-") {
		return PullRequest{}, fmt.Errorf("invalid PR base %q", base)
	}
	repoOrigin, err := gitText(ctx, repo, "remote", "get-url", "origin")
	if err != nil {
		return PullRequest{}, err
	}
	workOrigin, err := gitText(ctx, workdir, "remote", "get-url", "origin")
	if err != nil || repoOrigin != workOrigin {
		return PullRequest{}, errors.New("worktree origin does not match admitted repository")
	}
	if strings.TrimSpace(spec.Title) == "" {
		return PullRequest{}, errors.New("pull request title is required")
	}
	if strings.TrimSpace(spec.Body) == "" {
		return PullRequest{}, errors.New("pull request body is required")
	}
	var result struct {
		URL string `json:"url"`
	}
	input := map[string]any{"op": "open", "repo": repo, "workdir": workdir,
		"head": head, "base": base, "title": spec.Title, "body": spec.Body,
		"draft": spec.Draft}
	if expectedRemote != "" {
		input["expected_remote"] = expectedRemote
	}
	if err := f.execute(ctx, input, &result); err != nil {
		return PullRequest{}, err
	}
	if result.URL == "" {
		return PullRequest{}, errors.New("forge returned an empty PR reference")
	}
	return PullRequest{Ref: result.URL, URL: result.URL, Base: base, Head: head}, nil
}

func (f *HTTPForge) Mergeable(ctx context.Context, workdir, ref string) (bool, error) {
	number, err := pullNumber(ref)
	if err != nil {
		return false, err
	}
	var result struct {
		Mergeable int `json:"mergeable"`
	}
	if err := f.execute(ctx, map[string]any{"op": "info",
		"workdir": workdir, "number": number}, &result); err != nil {
		return false, err
	}
	return result.Mergeable == 1, nil
}

func (f *HTTPForge) CI(ctx context.Context, workdir, ref string) (CIState, error) {
	number, err := pullNumber(ref)
	if err != nil {
		return CIPending, err
	}
	var result struct {
		State CIState `json:"state"`
	}
	if err := f.execute(ctx, map[string]any{"op": "ci",
		"workdir": workdir, "number": number}, &result); err != nil {
		return CIPending, err
	}
	switch result.State {
	case CIPending, CIPassed, CIFailed:
		return result.State, nil
	default:
		return CIPending, fmt.Errorf("forge returned invalid CI state %q", result.State)
	}
}

func (f *HTTPForge) Merge(ctx context.Context, workdir, ref, requiredBase string) error {
	if !strings.HasPrefix(requiredBase, "aimee/feat/wi_") {
		return fmt.Errorf("refuse merge into unmanaged base %q", requiredBase)
	}
	number, err := pullNumber(ref)
	if err != nil {
		return err
	}
	var ignored map[string]any
	return f.execute(ctx, map[string]any{"op": "merge",
		"workdir": workdir, "number": number, "base": requiredBase}, &ignored)
}

func (f *HTTPForge) execute(ctx context.Context, input, output any) error {
	payload, err := json.Marshal(input)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		f.baseURL+"/v1/internal/forge/execute", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if f.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+f.bearer)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return readErr
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(data, &envelope); err != nil {
		return fmt.Errorf("decode forge response: %w", err)
	}
	var ok bool
	_ = json.Unmarshal(envelope["ok"], &ok)
	var code, responseError, detail string
	_ = json.Unmarshal(envelope["code"], &code)
	_ = json.Unmarshal(envelope["error"], &responseError)
	_ = json.Unmarshal(envelope["detail"], &detail)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || !ok {
		if code == "" {
			code = "publication_failed"
		}

		if detail == "" {
			detail = responseError
		}
		return &ForgePublicationError{Code: code, Detail: safeDiagnostic(detail)}
	}
	result, err := json.Marshal(envelope)
	if err != nil {
		return fmt.Errorf("encode forge response: %w", err)
	}
	if err := json.Unmarshal(result, output); err != nil {
		return fmt.Errorf("decode forge response: %w", err)
	}
	return nil
}

// mergeErrIsConflict reports whether a failed Merge describes a content
// conflict, which is terminal, rather than a lost race, which a retry wins.
//
// GitHub answers HTTP 405/409 for both. A lost race means head or base moved
// while we were merging ("Base branch was modified"); retrying wins it. A
// content conflict is a property of the two trees, so no retry can ever win and
// the step must fail instead of pending forever.
//
// Match the noun phrase "merge conflict" (this also covers "merge conflicts").
// The bare word "conflict" is deliberately NOT matched: HTTP 409 is literally
// named "Conflict" and its lost-race messages can carry that bare word, which
// would misclassify a winnable race as terminal.
//
// Fails safe: an unrecognised message is reported as NOT a conflict, preserving
// the retrying behaviour rather than rejecting work on a message we do not know.
func mergeErrIsConflict(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "merge conflict")
}

func managedBranch(branch string) bool {
	for _, prefix := range []string{"aimee/wi/wi_", "aimee/feat/wi_"} {
		if !strings.HasPrefix(branch, prefix) {
			continue
		}
		suffix := strings.TrimPrefix(branch, prefix)
		if suffix == "" || strings.Contains(suffix, "/") || strings.Contains(suffix, "..") {
			return false
		}
		for _, c := range suffix {
			if !(c == '_' || c == '.' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')) {
				return false
			}
		}
		return true
	}
	return false
}

func pullNumber(ref string) (int, error) {
	part := strings.TrimSpace(ref)
	if slash := strings.LastIndex(part, "/"); slash >= 0 {
		part = part[slash+1:]
	}
	number, err := strconv.Atoi(part)
	if err != nil || number <= 0 {
		return 0, fmt.Errorf("invalid pull request reference %q", ref)
	}
	return number, nil
}

// ghText remains only for the read-only default-branch fallback used while
// constructing local worktrees; authenticated WFE forge operations never use it.
func ghText(ctx context.Context, workdir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "gh", args...)
	cmd.Dir = workdir
	output, err := cmd.CombinedOutput()
	if err != nil {
		return string(output), fmt.Errorf("gh %s: %s", strings.Join(args, " "), strings.TrimSpace(string(output)))
	}
	return strings.TrimSpace(string(output)), nil
}
