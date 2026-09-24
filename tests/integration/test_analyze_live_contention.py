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
            save("state-events.json", [{"op": "fact", "run_id": "live@execute-a", "revision": 3, "created_at": at(10)},
                                       {"op": "complete", "run_id": "live@decide", "revision": 4, "created_at": at(19)}])
            save("validation.json", {"passed": True, "failures": []})
            save("http-observations.json", [{"request_id": 1, "run_id": "decide", "started_at": at(2), "finished_at": at(3), "http_status": 503, "usage": {"input_tokens": 1000}},
                                            {"request_id": 2, "run_id": "decide", "started_at": at(4), "finished_at": at(6), "http_status": 200,
                                             "errors": ["upstream_body_read_failed", "request_cancelled"],
                                             "usage": {"input_tokens": 7, "output_tokens": 3, "cache_read_input_tokens": 0}}])
            for run in runs:
                path = root / "workspace" / ".xloom" / "runs" / run["run_id"]
                path.mkdir(parents=True)
                (path / "job.json").write_text(json.dumps({"run_id": run["run_id"], "kind": run["kind"]}), encoding="utf-8")
                (path / "session.json").write_text("{}", encoding="utf-8")
                events = []
                if run["kind"] == "reason":
                    events = [{"type": "model_call_start", "at": at(1), "request": {"kind": "turn"}},
                              {"type": "model_call_end", "at": at(7), "request": {"kind": "turn", "duration_ms": 6000, "usage": {"input_tokens": 7, "output_tokens": 3, "cache_read_input_tokens": 100}}},
                              {"type": "message_end", "message": {"role": "assistant", "content": [{"type": "tool_use", "id": "commit-1", "input": {"op": "commit"}}]}},
                              {"type": "tool_start", "at": "2026-09-23T00:00:19.100000Z", "tool_id": "commit-1", "tool_name": "graph_action"},
                              {"type": "decision_operation", "text": json.dumps({"op": "decision_commit", "committed": True})},
                              {"type": "tool_end", "at": "2026-09-23T00:00:19.400000Z", "tool_id": "commit-1", "tool_name": "graph_action"}]
                (path / "events.jsonl").write_text("\n".join(json.dumps(event) for event in events), encoding="utf-8")
            report = analyze(root)
            manifest = json.loads((root / "manifest.json").read_text(encoding="utf-8"))
            manifest["http_observation_mode"] = "proxy"
            save("manifest.json", manifest)
            self.assertEqual(analyze(root), report)
            save("validation-reviewed.json", {"passed": True, "failures": [], "review_reason": "independent evidence review"})
            reviewed_report = analyze(root)
            path = root / "workspace" / ".xloom" / "runs" / "decide" / "events.jsonl"
            events = [json.loads(line) for line in path.read_text(encoding="utf-8").splitlines()]
            next(event for event in events if event["type"] == "model_call_end")["request"]["failed"] = True
            path.write_text("\n".join(json.dumps(event) for event in events), encoding="utf-8")
            failed_report = analyze(root)
        self.assertAlmostEqual(report["project_wall_seconds"], 19.4, places=5)
        self.assertEqual(report["authoritative_event_wall_seconds_lower_bound"], 19)
        self.assertEqual(report["end_basis"], "successful_complete_commit_receipt_observed")
        self.assertEqual(report["reported_usage"]["input_tokens"], 7)
        self.assertEqual(report["http_retries_in_matched_logical_calls"], 1)
        self.assertEqual(report["proxy_reported_usage"]["input_tokens"], 1007)
        self.assertEqual(report["usage_difference_calls"], 1)
        self.assertEqual(report["usage_comparisons"][0]["app_minus_proxy"]["cache_read_input_tokens"], 100)
        self.assertEqual(report["proxy_error_class_counts"]["http_attempt_failed"], 1)
        self.assertEqual(report["proxy_error_class_counts"]["stream_close_after_app_success"], 1)
        self.assertEqual(report["http_attempts"][1]["errors"], ["upstream_body_read_failed", "request_cancelled"])
        self.assertEqual(failed_report["proxy_error_class_counts"]["proxy_error_with_failed_logical_call"], 1)
        self.assertEqual(report["coverage"]["peak_concurrent_execute_runs"], 2)
        self.assertTrue(report["coverage"]["decision_saw_external_fact_updates"])
        self.assertEqual(len(report["external_business_updates_during_decisions"]), 1)
        self.assertTrue(report["coverage"]["all_run_evidence_present"])
        self.assertIn("本次工具响应未观测到 state_changed", render(report))
        self.assertIn("此统计不包含调度器主动取消过期决策", render(report))
        self.assertTrue(reviewed_report["validation_review_applied"])
        self.assertIn("原始 validation.json 未被覆盖", render(reviewed_report))
        self.assertIn("independent evidence review", render(reviewed_report))
        self.assertNotIn("marker suffix", render(reviewed_report))
        self.assertNotIn("arithmetic-correction", render(reviewed_report))

    def test_direct_mode_without_http_observations_keeps_application_usage(self):
        at = lambda second: f"2026-09-23T00:00:{second:02d}Z"
        for file_present in (False, True):
            with self.subTest(empty_file=file_present), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                (root / "manifest.json").write_text(json.dumps({
                    "started": at(0), "completed_observed": at(5), "http_observation_mode": "direct",
                }), encoding="utf-8")
                (root / "runs.json").write_text(json.dumps([
                    {"run_id": "direct-run", "kind": "reason", "started": at(1), "finished": at(4)},
                ]), encoding="utf-8")
                if file_present:
                    (root / "http-observations.json").write_text("[]", encoding="utf-8")
                path = root / "workspace" / ".xloom" / "runs" / "direct-run"
                path.mkdir(parents=True)
                events = [
                    {"type": "model_call_start", "at": at(1), "request": {"kind": "turn"}},
                    {"type": "model_call_end", "at": at(3), "request": {
                        "kind": "turn", "duration_ms": 2000,
                        "usage": {"input_tokens": 17, "output_tokens": 5},
                    }},
                ]
                (path / "events.jsonl").write_text("\n".join(json.dumps(event) for event in events), encoding="utf-8")
                report = analyze(root)
                self.assertEqual(report["http_observation_mode"], "direct")
                self.assertEqual(report["http_observation_status"], "not_collected")
                for key in ("http_attempt_count", "http_retries_in_matched_logical_calls", "unmatched_http_attempts",
                            "http_timing_ms", "http_stage_cumulative_seconds", "proxy_reported_usage",
                            "proxy_error_class_counts", "thinking_chars", "output_chars", "usage_difference_calls"):
                    self.assertIsNone(report[key], key)
                self.assertEqual(report["model_calls"], 1)
                self.assertEqual(report["usage_calls"], 1)
                self.assertEqual(report["usage_status"], "reported_only")
                self.assertEqual(report["reported_usage"]["input_tokens"], 17)
                self.assertEqual(report["reported_usage"]["output_tokens"], 5)
                self.assertEqual(report["timing"]["model_active_seconds"], 2)
                self.assertEqual(report["usage_comparisons"][0]["status"], "not_comparable")
                self.assertIsNone(report["usage_comparisons"][0]["app_minus_proxy"])
                rendered = render(report)
                self.assertIn("HTTP 观测未采集（direct 直连）", rendered)
                self.assertIn("| 代理各 HTTP 尝试（未采集） | 未知 | 未知 | 未知 | 未知 |", rendered)
                self.assertIn("| 应用逻辑请求 | 17 | 5 |", rendered)
                self.assertNotIn("HTTP 尝试 0 次", rendered)
                self.assertNotIn("到首事件等待 0.000 秒", rendered)
                self.assertNotIn("0 次存在差异", rendered)


if __name__ == "__main__":
    unittest.main()
