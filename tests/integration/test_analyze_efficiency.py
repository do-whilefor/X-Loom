import json
from pathlib import Path
import tempfile
import unittest

from analyze_efficiency import analyze, api_observations, input_composition, normalize_api_path, render, tool_analysis


def at(second):
    return f"2026-09-25T00:00:{second:02d}Z"


class EfficiencyAnalysisTests(unittest.TestCase):
    def minimal_evidence(self, root, completed=False):
        def save(name, value):
            (root / name).write_text(json.dumps(value), encoding="utf-8")
        manifest = {"started": at(0), "http_observation_mode": "proxy"}
        if completed:
            manifest["completed_observed"] = at(8)
        save("manifest.json", manifest)
        save("runs.json", [{"run_id": "run-a", "kind": "reason", "started": at(1), "finished": at(5)}])
        path = root / "workspace" / ".xloom" / "runs" / "run-a"
        path.mkdir(parents=True)
        events = [
            {"type": "model_call_start", "at": at(1), "request": {"kind": "turn"}},
            {"type": "model_call_end", "at": at(4), "request": {
                "kind": "turn", "duration_ms": 3000, "failed": True,
                "usage": {"input_tokens": 17, "output_tokens": 5}}},
        ]
        (path / "events.jsonl").write_text("\n".join(json.dumps(event) for event in events), encoding="utf-8")
        return save

    def test_incomplete_activity_window_is_not_project_completion_time(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            save = self.minimal_evidence(root)
            # A stopped final status alone has no trustworthy stop timestamp.
            save("state.json", {"graph": {"project": {"status": "stopped"}}})
            save("progress.json", {"at": at(9), "progress": "status=active revision=2"})
            unknown = analyze(root)
            self.assertFalse(unknown["project_completed"])
            self.assertIsNone(unknown["finished"])
            self.assertIsNone(unknown["project_wall_seconds"])
            self.assertIsNone(unknown["time_to_terminal_observation_seconds"])
            self.assertEqual(unknown["observed_activity_window_seconds"], 5)
            self.assertEqual(unknown["time_window_scope"], "observed_activity_only")
            self.assertEqual(unknown["timing"]["wall_seconds"], 5)
            self.assertIn("项目未完成；完整项目耗时未知", render(unknown))
            self.assertIn("观测活动窗口 **5.000 秒**", render(unknown))
            self.assertNotIn("项目墙钟 **5.000", render(unknown))
            self.assertNotIn("timing-report.json", render(unknown))
            save("progress.json", {"at": at(12), "progress": "status=stopped revision=3"})
            stopped = analyze(root)
            self.assertEqual(stopped["time_to_terminal_observation_seconds"], 12)
            self.assertEqual(stopped["terminal_observation_basis"], "terminal_status_progress_poll")
            self.assertIsNone(stopped["project_wall_seconds"])
            self.assertIn("轮询观察到终止状态为 12.000 秒", render(stopped))
            self.assertIn("不能当作成功任务耗时或精确停止时刻", render(stopped))

    def test_completed_project_retains_completion_window_and_report_link(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            self.minimal_evidence(root, completed=True)
            report = analyze(root)
        self.assertTrue(report["project_completed"])
        self.assertEqual(report["project_wall_seconds"], 8)
        self.assertEqual(report["observed_activity_window_seconds"], 8)
        self.assertEqual(report["time_window_scope"], "completed_project")
        self.assertIsNotNone(report["finished"])
        self.assertIn("项目墙钟 **8.000 秒**", render(report))
        self.assertIn("efficiency-report.json", render(report))
        self.assertNotIn("timing-report.json", render(report))

    def test_execution_cancellation_is_separate_from_tool_conflicts_and_usage_totals(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            save = self.minimal_evidence(root, completed=True)
            save("executions.json", [
                {"id": "run-a", "kind": "reason", "status": "failed", "result": {
                    "failure_kind": "state_changed", "error": "private-error"}, "job": "private-job"},
                {"id": "other", "kind": "reason", "status": "failed", "result": {"failure_kind": "provider"}},
                {"id": "worker", "kind": "explore", "status": "failed", "result": {"failure_kind": "state_changed"}},
                {"id": "missing", "kind": "reason", "status": "failed", "result": {"failure_kind": "state_changed"}},
            ])
            report = analyze(root)
        execution = report["execution_analysis"]
        self.assertEqual(execution["state_changed_decision_runs"], 2)
        self.assertEqual(execution["state_changed_decision_model_calls"], 1)
        self.assertEqual(execution["state_changed_decision_model_duration_ms"], 3000)
        self.assertEqual(execution["state_changed_decisions_without_run_evidence"], 1)
        self.assertEqual(execution["state_changed_decision_usage"]["reported_usage"]["input_tokens"], 17)
        self.assertEqual(report["reported_usage"]["input_tokens"], 17)
        self.assertEqual(report["coverage"]["state_changed_count"], 0)
        self.assertIn("state_changed 决策取消：2 个 run", render(report))
        self.assertIn("HTTP 402", render(report))
        self.assertNotIn("private-", json.dumps(report))

    def test_missing_observation_files_are_unknown_but_collected_empty_file_is_zero(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            save = self.minimal_evidence(root, completed=True)
            missing = analyze(root)
            self.assertEqual(missing["http_observation_status"], "not_collected")
            for key in ("http_attempt_count", "proxy_reported_usage", "http_timing_ms", "usage_difference_calls"):
                self.assertIsNone(missing[key], key)
            self.assertEqual(missing["reported_usage"]["input_tokens"], 17)
            self.assertEqual(missing["api_observations"]["status"], "not_collected")
            self.assertEqual(missing["execution_analysis"]["status"], "not_collected")
            self.assertIsNone(missing["execution_analysis"]["state_changed_decision_runs"])
            self.assertIn("HTTP 观测文件缺失", render(missing))
            self.assertNotIn("HTTP 尝试 0 次", render(missing))
            self.assertNotIn("direct", render(missing))
            save("http-observations.json", [])
            save("executions.json", [])
            empty = analyze(root)
            self.assertEqual(empty["http_observation_status"], "collected")
            self.assertEqual(empty["http_attempt_count"], 0)
            self.assertEqual(empty["execution_analysis"]["state_changed_decision_runs"], 0)

    def test_direct_run_keeps_application_usage_without_claiming_measured_input_composition(self):
        for file_present in (False, True):
            with self.subTest(empty_file=file_present), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                save = self.minimal_evidence(root, completed=True)
                save("manifest.json", {"started": at(0), "completed_observed": at(8),
                                       "http_observation_mode": "direct"})
                if file_present:
                    save("http-observations.json", [])
                report = analyze(root)
                self.assertEqual(report["http_observation_status"], "not_collected")
                self.assertIsNone(report["http_attempt_count"])
                self.assertEqual(report["input_composition"]["status"], "unknown")
                self.assertTrue(all(value is None for value in report["input_composition"]["byte_totals"].values()))
                self.assertIsNone(report["input_composition"]["repeated_history_fraction_of_messages"])
                self.assertEqual(report["reported_usage"]["input_tokens"], 17)
                text = render(report)
                self.assertIn("HTTP 观测未采集（direct 直连）", text)
                self.assertIn("请求输入组成或历史重传比例未知", text)
                self.assertIn("| body | 未知 |", text)
                self.assertNotIn("HTTP 尝试 0 次", text)
                self.assertNotIn("| body | 0 |", text)

    def test_input_bytes_deduplicate_observations_without_merging_runs(self):
        first = {"request_id": 1, "run_id": "a", "input_bytes": {"body": 200, "messages": 100, "repeated_message_bytes": 0}}
        repeat = {"request_id": 2, "run_id": "a", "input_bytes": {"body": 300, "messages": 200, "repeated_message_bytes": 80}}
        other = {"request_id": 1, "run_id": "b", "input_bytes": {"body": 200, "messages": 100, "repeated_message_bytes": 0}}
        report = input_composition([first, dict(first), repeat, other, {"request_id": 3, "run_id": "a"}])
        self.assertEqual(report["duplicate_observation_rows_excluded"], 1)
        self.assertEqual(report["requests"], 4)
        self.assertEqual(report["measured_requests"], 3)
        self.assertEqual(report["status"], "partial")
        self.assertEqual(report["byte_totals"]["body"], 700)
        self.assertEqual(report["byte_totals"]["messages"], 400)
        self.assertEqual(report["repeated_history_fraction_of_messages"], 0.2)
        self.assertEqual(report["by_run"]["a"]["byte_totals"]["repeated_message_bytes"], 80)
        self.assertEqual(report["by_run"]["b"]["byte_totals"]["repeated_message_bytes"], 0)
        self.assertIsNone(report["byte_totals"]["thinking_blocks"])
        self.assertIn("NOT tokens", report["basis"])

    def test_missing_composition_is_unknown_not_zero(self):
        for rows in ([], [{"request_id": 1}], [{"input_bytes": {"body": 0}}]):
            report = input_composition(rows)
            self.assertEqual(report["status"], "unknown")
            self.assertIsNone(report["byte_totals"]["body"])
            self.assertIsNone(report["repeated_history_fraction_of_messages"])
        mixed = input_composition([{"input_bytes": {"body": 10, "messages": 5}}, {"input_bytes": {"body": 10, "messages": 5, "repeated_message_bytes": 2}}])
        self.assertIsNone(mixed["repeated_history_fraction_of_messages"])

    def test_api_paths_percentiles_and_parallel_union(self):
        from analyze_live_contention import stamp
        start = stamp(at(0))
        rows = [
            {"method": "POST", "path": "/projects/private-a/executions/private-run-a/updates?token=private-query", "started": at(0), "duration_ms": 2000, "status": 200, "response_bytes": 100},
            {"method": "POST", "path": "/projects/private-b/executions/private-run-b/updates", "started": at(1), "duration_ms": 3000, "status": 503, "response_bytes": 50},
            {"method": "GET", "path": "/projects/private-a/state", "status": 200},
        ]
        report = api_observations(rows, (start, start + 3), True)
        self.assertAlmostEqual(report["active_wall_seconds"], 3)
        self.assertEqual(report["duration_ms"]["sum"], 4000)
        self.assertEqual(report["duration_ms"]["count"], 2)
        group = report["by_route"]["POST /projects/{pid}/executions/{rid}/updates"]
        self.assertEqual(group["calls"], 2)
        self.assertEqual(group["failed_calls"], 1)
        self.assertEqual(group["duration_ms"]["p95"], 2000)
        self.assertEqual(group["response_bytes"]["sum"], 150)
        self.assertNotIn("private", json.dumps(report))
        self.assertEqual(normalize_api_path("/projects/abc/rounds/3/entries/87"), "/projects/{pid}/rounds/{generation}/entries/{entry}")
        self.assertEqual(normalize_api_path("/executions/pending"), "/executions/pending")
        missing = api_observations([], (start, start + 1), False)
        self.assertEqual(missing["status"], "not_collected")
        self.assertIsNone(missing["active_wall_seconds"])
        self.assertEqual(missing["duration_ms"], {"count": 0})

    def test_observer_and_outside_window_are_not_production_costs(self):
        from analyze_live_contention import stamp
        start = stamp(at(0))
        rows = [{"method": method, "path": path, "started": at(second), "duration_ms": 100, "status": 200, "response_bytes": 10}
                for method, path, second in [("GET", "/projects/p/state", 1),
                                             ("GET", "/projects/p/state/events", 2),
                                             ("GET", "/projects/p/export", 3),
                                             ("POST", "/projects/p/state/read", 4),
                                             ("GET", "/projects/p/state/events", 10)]]
        report = api_observations(rows, (start, start + 10), True)
        self.assertEqual(report["calls"], 4)
        self.assertEqual(report["observer"]["calls"], 3)
        self.assertEqual(report["production"]["calls"], 1)
        self.assertEqual(report["production"]["duration_ms"]["sum"], 100)
        self.assertEqual(report["excluded_outside_project_window"], 1)
        self.assertIn("observer_event_export", report["by_category"])

    def test_tool_operations_failures_and_missing_inputs(self):
        runs = [{"run_id": "a", "tools": [
            {"name": "graph_action", "op": "fact", "finished": at(2), "duration_ms": 5},
            {"name": "graph_action", "op": "commit", "finished": at(3), "duration_ms": 9, "failed": True, "error_kind": "state_changed"},
            {"name": "bash", "finished": at(4), "duration_ms": 20},
            {"name": "read", "finished": at(4), "duration_ms": 1},
        ]}]
        with tempfile.TemporaryDirectory() as directory:
            report = tool_analysis(runs, Path(directory))
        self.assertEqual(report["by_operation"]["graph_action:fact"]["calls"], 1)
        self.assertEqual(report["by_operation"]["graph_action:commit"]["error_kinds"], {"state_changed": 1})
        self.assertEqual(report["by_operation"]["bash"]["duration_ms"]["sum"], 20)
        self.assertEqual(report["reads_without_input_metadata"], 1)

    def test_base_usage_stays_single_source_and_read_identity_is_scoped(self):
        usage = {"input_tokens": 10, "output_tokens": 3, "cache_read_input_tokens": 4}
        runs = [{"run_id": "planner", "kind": "reason", "started": at(0), "finished": at(9)},
                {"run_id": "worker", "kind": "explore", "started": at(1), "finished": at(8)}]
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)

            def save(name, value):
                (root / name).write_text(json.dumps(value), encoding="utf-8")

            save("manifest.json", {"started": at(0), "completed_observed": at(10), "validation_scope": "structural_report_delivery_only"})
            save("validation.json", {"passed": True})
            save("runs.json", runs)
            save("http-observations.json", [{"request_id": 1, "run_id": "planner", "started_at": at(1), "finished_at": at(5), "http_status": 200, "usage": {"input_tokens": 9999}}])
            for run in runs:
                path = root / "workspace" / ".xloom" / "runs" / run["run_id"]
                path.mkdir(parents=True)
                (path / "job.json").write_text(json.dumps({"kind": run["kind"]}), encoding="utf-8")
                (path / "session.json").write_text("{}", encoding="utf-8")
                offset = int(run["kind"] == "explore")
                events = [
                    {"type": "model_call_start", "at": at(1 + offset), "request": {"kind": "turn"}},
                    {"type": "model_call_end", "at": at(5 + offset), "request": {"kind": "turn", "duration_ms": 4000, "usage": usage}},
                    {"type": "message_end", "message": {"role": "assistant", "usage": usage, "content": [{"type": "thinking", "thinking": "private-thought-never-export"}]}},
                    {"type": "context_compaction_prepared", "compaction": {"usage": usage}},
                ]
                for index in range(3 if run["kind"] == "reason" else 1):
                    args = {"section": "facts", "ids": ["private-fact"]} if index % 2 == 0 else {"ids": ["private-fact"], "section": "facts"}
                    message = {"type": "message_end", "message": {"role": "assistant", "content": [{"type": "tool_use", "id": f"read-{index}", "name": "read_graph", "input": args}]}}
                    events.extend([message, message, {"type": "tool_start", "at": at(5 + index), "tool_id": f"read-{index}", "tool_name": "read_graph"}])
                    if index < 2:
                        event = {"type": "tool_end", "at": at(6 + index), "tool_id": f"read-{index}", "tool_name": "read_graph"}
                        if index == 0 and run["kind"] == "reason":
                            event["error"] = "state_changed: private-error-detail"
                        events.append(event)
                (path / "events.jsonl").write_text("\n".join(json.dumps(event) for event in events), encoding="utf-8")
            report = analyze(root)
        self.assertEqual(report["reported_usage"]["input_tokens"], 20)
        self.assertEqual(report["proxy_reported_usage"]["input_tokens"], 9999)
        self.assertEqual(report["run_kind_totals"]["reason"]["reported_usage"]["input_tokens"], 10)
        self.assertEqual(report["run_kind_totals"]["explore"]["model_duration_ms"], 4000)
        self.assertEqual(report["cumulative_model_duration_ms"], 8000)
        self.assertEqual(report["timing"]["model_active_seconds"], 5)
        self.assertEqual(len(report["run_costs"]), 2)
        tools = report["tool_analysis"]
        self.assertEqual(tools["by_operation"]["read_graph:facts"]["calls"], 4)
        self.assertEqual(tools["by_operation"]["read_graph:facts"]["failed_calls"], 1)
        self.assertEqual(tools["by_operation"]["read_graph:facts"]["incomplete_calls"], 1)
        self.assertEqual(tools["same_read_input_repeats"], 2)
        self.assertEqual(tools["same_read_input_after_previous_success"], 1)
        self.assertEqual(tools["reads_without_input_metadata"], 0)
        self.assertEqual(report["api_observations"]["status"], "not_collected")
        self.assertEqual(report["input_composition"]["status"], "unknown")
        encoded = json.dumps(report)
        self.assertNotIn("private-thought", encoded)
        self.assertNotIn("private-fact", encoded)
        self.assertNotIn("private-error-detail", encoded)
        self.assertIn("不是 token", render(report))
        self.assertIn("结构/报告交付验证：通过", render(report))
        self.assertNotIn("业务验收：通过", render(report))


if __name__ == "__main__":
    unittest.main()
