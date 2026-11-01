package miner

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
)

const maxReportBytes = 128 * 1024 * 1024

// Save atomically replaces a report with a private file. Existing symlinks and
// non-regular destinations are refused; session files are never changed.
func Save(path string, report *Report) error {
	return saveReport(path, report, func(directory *os.File) error { return directory.Sync() })
}

// DurabilityError means replacement succeeded, but directory sync failed. The
// caller must keep the new state: reporting a rollback would disagree with disk.
type DurabilityError struct{ Err error }

func (e *DurabilityError) Error() string {
	return "report was replaced, but its durability could not be confirmed: " + e.Err.Error()
}

func (e *DurabilityError) Unwrap() error { return e.Err }

func saveReport(path string, report *Report, syncDirectory func(*os.File) error) error {
	if err := validateReport(report); err != nil {
		return err
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	if len(data) > maxReportBytes {
		return errors.New("report too large")
	}
	if sessionIDPattern.MatchString(strings.TrimSuffix(filepath.Base(path), ".json")) && strings.HasSuffix(path, ".json") {
		sourceDir, sourceErr := os.Stat(report.SourceDirectory)
		targetDir, targetErr := os.Stat(filepath.Dir(path))
		if sourceErr == nil && targetErr == nil && os.SameFile(sourceDir, targetDir) {
			return errors.New("cannot save a report over a session file")
		}
	}
	if err := checkDestination(path); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".miner-report-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err := f.Chmod(0600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := checkDestination(path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	if err := os.Rename(name, path); err != nil {
		return err
	}
	if err := syncDirectory(dir); err != nil {
		return &DurabilityError{Err: err}
	}
	return nil
}

func checkDestination(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("report destination must be a regular file, not a symlink")
	}
	return nil
}

// Load validates the portable report rather than trusting its saved decisions.
func Load(path string) (*Report, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxReportBytes {
		return nil, errors.New("invalid report file")
	}
	data, err := io.ReadAll(io.LimitReader(f, maxReportBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxReportBytes {
		return nil, errors.New("report too large")
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	var report Report
	if err := dec.Decode(&report); err != nil {
		return nil, fmt.Errorf("decode report: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, errors.New("trailing report data")
	}
	if err := validateReport(&report); err != nil {
		return nil, err
	}
	return &report, nil
}

func validateReport(r *Report) error {
	if r == nil || r.Version != Version || r.CreatedAt.IsZero() || r.SourceDirectory == "" || r.Focus == "" {
		return errors.New("invalid report header")
	}
	if len(r.Candidates) > maxCandidates || len(r.Tools) > 10000 {
		return errors.New("report exceeds limits")
	}
	if r.Stats.Sessions < 0 || r.Stats.Turns < 0 || r.Stats.Duplicates < 0 || r.Stats.Omitted < 0 {
		return errors.New("negative scan statistics")
	}
	for key, value := range r.Stats.Skipped {
		if key == "" || value < 0 {
			return errors.New("invalid skipped statistics")
		}
	}
	if r.Usage.Input < 0 || r.Usage.Output < 0 || r.Usage.CacheRead < 0 || r.Usage.CacheWrite < 0 {
		return errors.New("negative token usage")
	}
	definitions := map[string]bool{}
	for _, d := range r.Tools {
		if d.Name == "" || definitions[d.Name] {
			return errors.New("invalid tool definitions")
		}
		definitions[d.Name] = true
	}
	if SensitiveText(r.Focus) || sensitiveJSON(r.Focus) {
		return errors.New("the focus looks like it contains a secret; rephrase it")
	}
	if SensitiveText(r.SourceDirectory) || SensitiveText(r.Workspace) || SensitiveText(r.Judge) {
		return errors.New("sensitive report metadata")
	}
	toolJSON, err := json.Marshal(r.Tools)
	if err != nil || SensitiveText(string(toolJSON)) {
		return errors.New("invalid or sensitive tool definitions")
	}
	ids := map[string]bool{}
	for _, c := range r.Candidates {
		if c.ID == "" || ids[c.ID] || !sessionIDPattern.MatchString(c.Source.SessionID) || !sessionIDPattern.MatchString(c.Source.GroupID) || c.Source.Start < 0 || c.Source.End-c.Source.Start != len(c.Messages) || c.Source.UpdatedAt.IsZero() {
			return errors.New("invalid candidate source")
		}
		ids[c.ID] = true
		for _, s := range []string{c.ID, c.Source.Title, c.Source.Directory, c.Source.Provider, c.Source.Model, c.Feedback, c.JudgeError} {
			if SensitiveText(s) {
				return errors.New("sensitive candidate metadata")
			}
		}
		if imagePattern.MatchString(c.Feedback) {
			return errors.New("image in feedback")
		}
		if sensitiveJSON(c.Feedback) {
			return errors.New("sensitive candidate metadata")
		}
		b, err := json.Marshal(c.Messages)
		feedbackJSON, feedbackErr := json.Marshal(c.Feedback)
		if err != nil || feedbackErr != nil || len(b)+len(feedbackJSON) > maxTurnBytes {
			return errors.New("candidate too large")
		}
		if err := ValidateMessages(c.Messages, r.Tools); err != nil {
			return fmt.Errorf("invalid candidate %s: %w", c.ID, err)
		}
		for _, s := range c.Signals {
			if s == "" || SensitiveText(s) {
				return errors.New("invalid candidate signal")
			}
		}
		if c.Assessment != nil {
			if err := validateAssessment(*c.Assessment); err != nil {
				return fmt.Errorf("invalid candidate assessment: %w", err)
			}
		}
		switch c.Review.Decision {
		case "keep":
			if c.Review.Rank < 1 || c.Review.Rank > 5 || c.JudgeError != "" || ExportBlock(c) != "" {
				return errors.New("invalid keep decision")
			}
		case "", "reject", "defer":
			if c.Review.Rank != 0 {
				return errors.New("rank without keep decision")
			}
		default:
			return errors.New("invalid review decision")
		}
	}
	return nil
}

// Markdown summarizes a report without embedding its private transcripts.
func Markdown(r *Report) string {
	if r == nil {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# Mining report\n\nCreated: %s  \nSource: %s  \nWorkspace: %s\n\n", r.CreatedAt.Format("2006-01-02 15:04:05 MST"), markdownText(r.SourceDirectory), markdownText(r.Workspace))
	fmt.Fprintf(&b, "Focus: %s\n\nJudge: %s\n\nHistorical instructions and tool schemas are unavailable. Export uses a normalized policy and the tool definitions captured at scan time.\n\n", markdownText(r.Focus), markdownText(r.Judge))
	fmt.Fprintf(&b, "Sessions: %d · Turns: %d · Candidates: %d · Duplicates: %d · Omitted: %d\n\n", r.Stats.Sessions, r.Stats.Turns, len(r.Candidates), r.Stats.Duplicates, r.Stats.Omitted)
	keys := make([]string, 0, len(r.Stats.Skipped))
	for k := range r.Stats.Skipped {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "- Skipped %s: %d\n", markdownText(k), r.Stats.Skipped[k])
	}
	fmt.Fprintf(&b, "\nJudge usage: input %d, cache read %d, cache write %d, output %d tokens. Cost: not recorded.\n", r.Usage.Input, r.Usage.CacheRead, r.Usage.CacheWrite, r.Usage.Output)
	for i, c := range r.Candidates {
		fmt.Fprintf(&b, "\n## %d. %s\n\n", i+1, markdownText(c.ID))
		fmt.Fprintf(&b, "- Source: %s · group %s · messages [%d, %d) · %s\n", markdownText(c.Source.SessionID), markdownText(c.Source.GroupID), c.Source.Start, c.Source.End, c.Source.UpdatedAt.Format("2006-01-02 15:04:05 MST"))
		fmt.Fprintf(&b, "- Title: %s · Directory: %s · Provider/model: %s / %s\n", markdownText(c.Source.Title), markdownText(c.Source.Directory), markdownText(c.Source.Provider), markdownText(c.Source.Model))
		signals := make([]string, len(c.Signals))
		for i, s := range c.Signals {
			signals[i] = markdownText(s)
		}
		fmt.Fprintf(&b, "- Signals: %s · Priority: %d\n", strings.Join(signals, ", "), c.Priority)
		if c.Assessment != nil {
			a := c.Assessment
			fmt.Fprintf(&b, "- Assessment: %s · %s · %s · missing context: %t · sensitive: %t\n", markdownText(a.Recommendation), markdownText(a.Category), markdownText(a.Summary), a.MissingContext, a.Sensitive)
			fmt.Fprintf(&b, "- Reason: %s\n- Scores: initiative %d/5 · completion %d/5 · restraint %d/5\n", markdownText(a.Reason), a.Initiative, a.Completion, a.Restraint)
			for _, evidence := range a.Evidence {
				fmt.Fprintf(&b, "  - %s\n", markdownText(evidence))
			}
		}
		if c.JudgeError != "" {
			fmt.Fprintf(&b, "- Judge error: %s\n", markdownText(c.JudgeError))
		}
		fmt.Fprintf(&b, "- Review: %s · Rank: %d\n", markdownText(c.Review.Decision), c.Review.Rank)
	}
	return b.String()
}

func markdownText(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			b.WriteByte(' ')
			continue
		}
		if strings.ContainsRune(`\\`+"`*_{}[]<>#|!", r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}
