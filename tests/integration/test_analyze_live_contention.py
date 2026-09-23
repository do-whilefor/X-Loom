import json
from pathlib import Path
import tempfile
import unittest

from analyze_live_contention import analyze, analyze_run, peak_concurrency, render, timeline, usage_totals


class TimingAnalysisTests(unittest.TestCase):
    def test_parallel_intervals_count_once_and_clip(self):
        result = timeline([(0, 5), (2, 7), (9, 14)], [(4, 8), (5, 6)], (1, 11))
        self.assertEqual(result["wall_seconds"], 10)
        self.assertEqual(result["model_active_seconds"], 8)
        self.assertEqual(result["tool_active_seconds"], 4)
        self.assertEqual(result["overlap_seconds"], 3)
        self.assertEqual(result["model_only_seconds"], 5)
        self.assertEqual(result["tool_only_seconds"], 1)
        self.assertEqual(result["neither_seconds"], 1)
        self.assertEqual(peak_concurrency([(0, 2), (1, 3), (3, 5)]), 2)

    def test_request_usage_counted_once_despite_transcript_copy(self):
        at = lambda second: f"2026-09-23T00:00:{second:02d}Z"
        usage = {"input_tokens": 10, "output_tokens": 4}
        events = [
            {"type": "model_call_start", "at": at(1), "request": {"kind": "turn"}},
            {"type": "model_call_end", "at": at(4), "request": {"kind": "turn", "duration_ms": 3000, "usage": usage}},
            {"type": "message_end", "message": {"role": "assistant", "usage": usage, "content": [{"type": "tool_use", "id": "read-1", "name": "read_graph", "input": {"section": "facts", "private": "do not retain"}}]}},
            {"type": "tool_start", "at": at(4), "tool_id": "read-1", "tool_name": "read_graph"},
            {"type": "decision_operation", "text": json.dumps({"op": "read_graph", "state_changed": True, "failed": True})},
            {"type": "tool_end", "at": at(5), "tool_id": "read-1", "tool_name": "read_graph", "error": "state_changed: hidden server detail"},
            {"type": "model_call_start", "at": at(6), "request": {"kind": "summary"}},
            {"type": "model_call_end", "at": at(8), "request": {"kind": "summary", "duration_ms": 2000, "failed": True, "usage": usage}},
            {"type": "context_compaction_prepared", "compaction": {"usage": usage}},
        ]
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory)
            (path / "events.jsonl").write_text("\n".join(json.dumps(event) for event in events), encoding="utf-8")
            result, model, tools = analyze_run(path, {"run_id": "r1", "kind": "reason", "started": at(0), "finished": at(9)})
        self.assertEqual(result["model_calls"], 2)
        self.assertEqual(result["summary_calls"], 1)
        self.assertEqual(result["failed_model_calls"], 1)
        self.assertEqual(result["reported_usage"]["input_tokens"], 20)
        self.assertEqual(result["reported_usage"]["output_tokens"], 8)
        self.assertEqual(result["usage_calls"], 2)
        self.assertEqual(result["tools"][0]["section"], "facts")
        self.assertEqual(result["tools"][0]["model_requests_after_conflict"], [2])
        self.assertEqual(result["tools"][0]["duration_ms"], 1000)
        self.assertEqual(len(model), 2)
        self.assertEqual(len(tools), 1)
        self.assertNotIn("private", json.dumps(result))
        self.assertNotIn("hidden server detail", json.dumps(result))
        self.assertEqual(usage_totals([{"usage": usage}, {}])["usage_status"], "partial")

    def test_complete_fixture_has_external_updates_and_http_retry(self):
        at = lambda second: f"2026-09-23T00:00:{second:02d}Z"
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            def save(name, value):
                (root / name).write_text(json.dumps(value), encoding="utf-8")
            save("manifest.json", {"started": at(0), "completed_observed": at(21), "model": "fixture", "source_commit": "test", "reasoning_effort": "max"})
            runs = [{"run_id": "decide", "kind": "reason", "started": at(1), "finished": at(20), "status": "success"},
                    {"run_id": "execute-a", "kind": "explore", "started": at(4), "finished": at(12), "status": "success"},
                    {"run_id": "execute-b", "kind": "explore", "started": at(8), "finished": at(16), "status": "success"}]
            save("runs.json", runs)
            save("state-events.json", [{"op": "fact", "run_id": "execute-a", "revision": 3, "created_at": at(10)},
                                       {"op": "complete", "run_id": "decide", "revision": 4, "created_at": at(19)}])
            save("validation.json", {"passed": True, "failures": []})
            save("http-observations.json", [{"request_id": 1, "run_id": "decide", "started_at": at(2), "finished_at": at(3), "http_status": 503, "usage": {"input_tokens": 1000}},
                                            {"request_id": 2, "run_id": "decide", "started_at": at(4), "finished_at": at(6), "http_status": 200, "usage": {"input_tokens": 1000}}])
            for run in runs:
                path = root / "workspace" / ".xloom" / "runs" / run["run_id"]
                path.mkdir(parents=True)
                (path / "job.json").write_text(json.dumps({"run_id": run["run_id"], "kind": run["kind"]}), encoding="utf-8")
                (path / "session.json").write_text("{}", encoding="utf-8")
                events = []
                if run["kind"] == "reason":
                    events = [{"type": "model_call_start", "at": at(1), "request": {"kind": "turn"}},
                              {"type": "model_call_end", "at": at(7), "request": {"kind": "turn", "duration_ms": 6000, "usage": {"input_tokens": 7, "output_tokens": 3}}}]
                (path / "events.jsonl").write_text("\n".join(json.dumps(event) for event in events), encoding="utf-8")
            report = analyze(root)
        self.assertEqual(report["project_wall_seconds"], 19)
        self.assertEqual(report["reported_usage"]["input_tokens"], 7)
        self.assertEqual(report["http_retries_in_matched_logical_calls"], 1)
        self.assertEqual(report["coverage"]["peak_concurrent_execute_runs"], 2)
        self.assertTrue(report["coverage"]["decision_saw_external_fact_updates"])
        self.assertTrue(report["coverage"]["all_run_evidence_present"])
        self.assertIn("本次没有触发版本冲突", render(report))


if __name__ == "__main__":
    unittest.main()
