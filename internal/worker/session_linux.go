//go:build linux

package worker

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"xloom/internal/agent"
	"xloom/internal/board"
	"xloom/internal/config"
)

const sessionSchemaVersion = 1
const maxRunRecoveries = 2

type executionIdentity struct {
	ProjectID       string      `json:"project_id"`
	StepID          string      `json:"step_id,omitempty"`
	RunID           string      `json:"run_id"`
	PreviousRunID   string      `json:"previous_run_id,omitempty"`
	Kind            string      `json:"kind"`
	Workspace       string      `json:"workspace"`
	RunDir          string      `json:"run_dir"`
	WorkspaceTarget string      `json:"workspace_target"`
	RunDirTarget    string      `json:"run_dir_target"`
	JobDigest       string      `json:"job_digest"`
	OriginalBudget  config.Task `json:"original_budget"`
}

// Paths are bound both as supplied (after Abs/Clean) and as resolved targets:
// changing a symlink cannot silently redirect a resumed execution.
func identityFor(j Job, runDir string) (executionIdentity, error) {
	w, err := filepath.EvalSymlinks(j.Workspace)
	if err != nil {
		return executionIdentity{}, fmt.Errorf("resolve workspace: %w", err)
	}
	r, err := filepath.EvalSymlinks(runDir)
	if err != nil {
		return executionIdentity{}, fmt.Errorf("resolve run directory: %w", err)
	}
	raw, err := json.Marshal(j)
	if err != nil {
		return executionIdentity{}, err
	}
	sum := sha256.Sum256(raw)
	i := executionIdentity{ProjectID: j.Graph.Project.ID, RunID: j.RunID, PreviousRunID: j.PreviousRunID, Kind: j.Kind, Workspace: j.Workspace, RunDir: runDir, WorkspaceTarget: w, RunDirTarget: r, JobDigest: hex.EncodeToString(sum[:]), OriginalBudget: j.Budget}
	if j.Intent != nil {
		i.StepID = j.Intent.ID
	}
	return i, nil
}

type journalCheckpoint struct {
	Offset int64  `json:"offset"`
	SHA256 string `json:"sha256"`
}

type session struct {
	SchemaVersion          int                      `json:"schema_version"`
	Identity               executionIdentity        `json:"identity"`
	Log                    journalCheckpoint        `json:"log_checkpoint"`
	ContextCheckpoint      *agent.ContextCheckpoint `json:"context_checkpoint,omitempty"`
	DecisionMetrics        *DecisionMetrics         `json:"decision_metrics,omitempty"`
	Replan                 *ReplanObservation       `json:"replan,omitempty"`
	GraphVersion           string                   `json:"graph_version,omitempty"`
	RecoveryCount          int                      `json:"recovery_count"`
	Phase                  string                   `json:"phase"`
	RunID                  string                   `json:"run_id"`
	Kind                   string                   `json:"kind"`
	StartedAt              time.Time                `json:"started_at"`
	ExecutionDeadline      time.Time                `json:"execution_deadline,omitempty"`
	ReasonDeadline         time.Time                `json:"reason_deadline,omitempty"`
	ConcludeStartedAt      time.Time                `json:"conclude_started_at,omitempty"`
	ConcludeDeadline       time.Time                `json:"conclude_deadline,omitempty"`
	Concluding             bool                     `json:"concluding"`
	History                []agent.Message          `json:"history"`
	Result                 *Result                  `json:"result,omitempty"`
	TaskPrompt             string                   `json:"task_prompt,omitempty"`
	ConclusionPrompt       string                   `json:"conclusion_prompt,omitempty"`
	ConclusionInputVersion int                      `json:"conclusion_input_version,omitempty"`
	ConclusionEvidence     []board.EvidenceRef      `json:"conclusion_evidence,omitempty"`
	Repairing              bool                     `json:"repairing,omitempty"`
	RepairCount            int                      `json:"repair_count,omitempty"`
	RepairReason           string                   `json:"repair_reason,omitempty"`
	RepairPrompt           string                   `json:"repair_prompt,omitempty"`
	RepairPending          bool                     `json:"repair_pending,omitempty"`
	ContinuationCount      int                      `json:"continuation_count,omitempty"`
	ContinuationSequence   uint64                   `json:"continuation_sequence,omitempty"`
}

func (s *session) validate(i executionIdentity) error {
	// Unversioned sessions cannot prove which immutable input produced their
	// messages/results. Preserve them for audit; never invent identity on load.
	if s.SchemaVersion != sessionSchemaVersion {
		return fmt.Errorf("unsupported session schema_version %d; automatic migration of unbound sessions is disabled", s.SchemaVersion)
	}
	a, _ := json.Marshal(s.Identity)
	b, _ := json.Marshal(i)
	if !bytes.Equal(a, b) || s.RunID != i.RunID || s.Kind != i.Kind || s.StartedAt.IsZero() {
		return errors.New("session task identity or immutable input mismatch")
	}
	if s.RepairCount < 0 || s.RepairCount > maxOutputRepairs || (s.Repairing && (s.RepairCount == 0 || s.RepairPrompt == "")) || (!s.Repairing && (s.RepairPending || s.RepairPrompt != "")) {
		return errors.New("invalid saved result-repair state")
	}
	if s.ContinuationCount < 0 || s.ContinuationCount > maxContinuations {
		return errors.New("invalid saved continuation count")
	}
	if s.RecoveryCount < 0 || s.RecoveryCount > maxRunRecoveries {
		return errors.New("invalid saved recovery count")
	}
	if s.ExecutionDeadline.IsZero() != (i.OriginalBudget.Timeout == 0) {
		return errors.New("missing original execution deadline")
	}
	if i.OriginalBudget.Timeout > 0 && !s.ExecutionDeadline.Equal(s.StartedAt.Add(time.Duration(i.OriginalBudget.Timeout)*time.Second)) {
		return errors.New("execution deadline differs from original budget")
	}
	if s.Concluding && (s.ConcludeStartedAt.IsZero() || s.ConcludeDeadline.IsZero()) {
		return errors.New("conclusion lacks its original deadline")
	}
	if !s.ReasonDeadline.IsZero() && (s.ExecutionDeadline.IsZero() || s.ReasonDeadline.After(s.ExecutionDeadline)) {
		return errors.New("reason deadline exceeds the original execution budget")
	}
	if s.Concluding && !s.ConcludeDeadline.Equal(s.ConcludeStartedAt.Add(time.Duration(i.OriginalBudget.ConcludeTimeout)*time.Second)) {
		return errors.New("conclusion deadline differs from original budget")
	}
	return nil
}

func (s *session) save(runDir string, journal *eventJournal) error {
	if err := journal.file.Sync(); err != nil {
		return err
	}
	s.Log = journal.checkpoint()
	if s.Kind == "reason" {
		metrics := journal.metrics
		s.DecisionMetrics = &metrics
	}
	s.Phase = "execute"
	if s.Concluding {
		s.Phase = "conclude"
	}
	if s.Repairing {
		s.Phase = "repair"
	}
	if s.Result != nil {
		s.Phase = "terminal"
		if s.Result.Retryable {
			s.Phase = "recoverable"
		}
	}
	raw, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return atomicSessionFile(runDir, raw)
}

func syncDirectory(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}

func atomicSessionFile(runDir string, raw []byte) error {
	f, err := os.CreateTemp(runDir, "session-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(raw); err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	if err = os.Rename(f.Name(), filepath.Join(runDir, "session.json")); err != nil {
		return err
	}
	return syncDirectory(runDir)
}

type eventJournal struct {
	file           *os.File
	digest         hash.Hash
	offset         int64
	lastSequence   uint64
	lastCompaction uint64
	uncommitted    int64
	partialArchive string
	metrics        DecisionMetrics
}

// Complete raw records are append-only. Only an incomplete, uncommitted final
// record may be removed, after its exact bytes have been synced to an archive.
func openJournal(runDir string, saved *journalCheckpoint) (*eventJournal, error) {
	f, err := os.OpenFile(filepath.Join(runDir, "events.jsonl"), os.O_CREATE|os.O_RDWR|os.O_APPEND, 0600)
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			f.Close()
		}
	}()
	j := &eventJournal{file: f, digest: sha256.New(), metrics: newDecisionMetrics()}
	stat, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if saved == nil && stat.Size() > 0 {
		return nil, errors.New("event history exists without a bound session")
	}
	if saved != nil && (saved.Offset < 0 || saved.Offset > stat.Size()) {
		return nil, errors.New("event log is shorter than its committed checkpoint")
	}
	verified := saved == nil
	verify := func() error {
		if saved != nil && j.offset == saved.Offset {
			if hex.EncodeToString(j.digest.Sum(nil)) != saved.SHA256 {
				return errors.New("committed event log checksum mismatch")
			}
			verified = true
		}
		return nil
	}
	if err = verify(); err != nil {
		return nil, err
	}
	r := bufio.NewReader(f)
	for {
		line, readErr := r.ReadBytes('\n')
		if readErr != nil && readErr != io.EOF {
			return nil, readErr
		}
		if len(line) == 0 {
			break
		}
		if readErr == io.EOF {
			if !verified {
				return nil, errors.New("committed event checkpoint includes a partial record")
			}
			archive, err := os.CreateTemp(runDir, "events-incomplete-*.jsonl")
			if err != nil {
				return nil, err
			}
			if _, err = archive.Write(line); err == nil {
				err = archive.Sync()
			}
			err = errors.Join(err, archive.Close())
			if err != nil {
				return nil, err
			}
			if err = syncDirectory(runDir); err != nil {
				return nil, err
			}
			if err = f.Truncate(j.offset); err != nil {
				return nil, err
			}
			if err = f.Sync(); err != nil {
				return nil, err
			}
			j.partialArchive = filepath.Base(archive.Name())
			break
		}
		var event agent.Event
		if err = json.Unmarshal(line, &event); err != nil || event.Type == "" {
			return nil, fmt.Errorf("invalid complete event record at byte %d", j.offset)
		}
		if !strings.HasPrefix(event.Type, "replan_") && event.Message != nil && event.Message.Sequence > j.lastSequence {
			j.lastSequence = event.Message.Sequence
		}
		if !strings.HasPrefix(event.Type, "replan_") && event.Compaction != nil && event.Compaction.ID > j.lastCompaction {
			j.lastCompaction = event.Compaction.ID
		}
		j.metrics.observe(event)
		j.digest.Write(line)
		j.offset += int64(len(line))
		if err = verify(); err != nil {
			return nil, err
		}
	}
	if !verified {
		return nil, errors.New("event checkpoint is not at a record boundary")
	}
	if saved != nil {
		j.uncommitted = j.offset - saved.Offset
	}
	ok = true
	return j, nil
}

func (j *eventJournal) checkpoint() journalCheckpoint {
	return journalCheckpoint{Offset: j.offset, SHA256: hex.EncodeToString(j.digest.Sum(nil))}
}

func (j *eventJournal) append(v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	n, err := j.file.Write(raw)
	if err != nil {
		return err
	}
	if n != len(raw) {
		return io.ErrShortWrite
	}
	j.digest.Write(raw)
	j.offset += int64(n)
	if event, ok := v.(agent.Event); ok {
		j.metrics.observe(event)
	}
	return nil
}
