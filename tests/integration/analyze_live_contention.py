"""Analyze retained live contention evidence without copying model content."""
from __future__ import annotations

import argparse
from collections import Counter
from datetime import datetime, timezone
import json
import math
from pathlib import Path


USAGE_KEYS = ("input_tokens", "output_tokens", "cache_read_input_tokens", "cache_creation_input_tokens")
BUSINESS_OPS = {"fact", "step_completed", "fact_relation", "finding", "goal", "step", "complete"}


def read_json(path, default=None):
    return json.loads(path.read_text(encoding="utf-8-sig")) if path.exists() else default


def stamp(value):
    if not value or value.startswith("0001-"):
        return None
    return datetime.fromisoformat(value.replace("Z", "+00:00")).timestamp()


def iso(value):
    return datetime.fromtimestamp(value, timezone.utc).isoformat() if value is not None else None


def merge_intervals(intervals, window=None):
    usable = []
    for start, end in intervals:
        if start is None or end is None:
            continue
        if window:
            start, end = max(start, window[0]), min(end, window[1])
        if end > start:
            usable.append((start, end))
    merged = []
    for start, end in sorted(usable):
        if merged and start <= merged[-1][1]:
            merged[-1] = (merged[-1][0], max(end, merged[-1][1]))
        else:
            merged.append((start, end))
    return merged


def interval_seconds(intervals):
    return sum(end - start for start, end in intervals)


def timeline(model, tool, window):
    models, tools = merge_intervals(model, window), merge_intervals(tool, window)
    union = merge_intervals(models + tools)
    m, t, busy = map(interval_seconds, (models, tools, union))
    overlap = max(0, m + t - busy)
    return {"wall_seconds": window[1] - window[0], "model_active_seconds": m,
            "tool_active_seconds": t, "overlap_seconds": overlap,
            "model_only_seconds": max(0, m - overlap), "tool_only_seconds": max(0, t - overlap),
            "neither_seconds": max(0, window[1] - window[0] - busy),
            "basis": "interval union clipped to project window; overlapping runs are counted once"}


def peak_concurrency(intervals):
    points = [(start, 1) for start, end in intervals if start is not None and end is not None and end > start]
    points += [(end, -1) for start, end in intervals if start is not None and end is not None and end > start]
    active = peak = 0
    for _, change in sorted(points):
        active += change
        peak = max(peak, active)
    return peak


def stats(values):
    values = sorted(value for value in values if value is not None)
    if not values:
        return {"count": 0}
    return {"count": len(values), "sum": sum(values), "min": values[0], "max": values[-1],
            "mean": sum(values) / len(values),
            **{f"p{p}": values[max(0, math.ceil(len(values) * p / 100) - 1)] for p in (50, 95, 99)},
            "percentile_basis": "nearest rank"}


def error_kind(error):
    text = (error or "").lower()
    for key in ("state_changed", "budget_exhausted", "deadline exceeded", "context canceled"):
        if key in text:
            return key.replace(" ", "_")
    return "other" if error else None


def usage_totals(requests):
    # Model request ends are the single source. Message, compaction and HTTP
    # copies of that usage must never be added to the same total.
    total = {key: 0 for key in USAGE_KEYS}
    reported = 0
    for request in requests:
        if request.get("usage") is not None:
            reported += 1
            for key in USAGE_KEYS:
                total[key] += request["usage"].get(key, 0)
    return {"reported_usage": total, "usage_calls": reported,
            "usage_status": "reported_only" if requests and reported == len(requests) else "partial" if reported else "unknown",
            "usage_basis": "model_call_end only; returned provider usage, not a verified bill"}


def analyze_run(directory, observed):
    job = read_json(directory / "job.json", {})
    session = read_json(directory / "session.json", {})
    path = directory / "events.jsonl"
    events = [json.loads(line) for line in path.read_text(encoding="utf-8").splitlines() if line.strip()] if path.exists() else []
    run_id = observed.get("run_id") or job.get("run_id") or directory.name
    started = stamp(observed.get("started") or session.get("started_at"))
    finished = stamp(observed.get("finished"))
    snapshot = job.get("input_snapshot") or {}
    result = session.get("result") or {}
    report = {"run_id": run_id, "kind": observed.get("kind") or job.get("kind"),
              "step_id": observed.get("step_id") or (job.get("intent") or {}).get("id"),
              "status": observed.get("status") or result.get("status"),
              "failure_kind": observed.get("failure_kind") or result.get("failure_kind"),
              "runner_error": observed.get("runner_error", False),
              "started": iso(started), "finished": iso(finished),
              "wall_seconds": finished - started if started is not None and finished is not None else None,
              "input_revision": observed.get("input_revision", snapshot.get("revision")),
              "input_version": observed.get("input_version") or snapshot.get("state_version"),
              "recovery_count": session.get("recovery_count", 0), "repair_count": session.get("repair_count", 0),
              "requests": [], "tools": [], "decision_operations": [], "warnings": []}
    report["missing_evidence_files"] = [name for name in ("job.json", "session.json", "events.jsonl") if not (directory / name).exists()]
    tool_inputs = {}
    for event in events:
        message = event.get("message") or {}
        if event.get("type") == "message_end" and message.get("role") == "assistant":
            for block in message.get("content", []):
                if block.get("type") == "tool_use":
                    args = block.get("input") or {}
                    # Retain protocol selectors only, never commands or model text.
                    tool_inputs[block.get("id")] = {key: args[key] for key in ("op", "section") if key in args}
    pending_models, pending_tools = {}, {}
    model_intervals, tool_intervals = [], []
    for index, event in enumerate(events):
        original_type = event.get("type", "")
        shadow = original_type.startswith("replan_")
        kind = original_type.removeprefix("replan_")
        at = stamp(event.get("at"))
        stream = "replan" if shadow else "main"
        if kind == "model_call_start":
            if stream in pending_models:
                report["warnings"].append("unpaired_model_start")
            request = event.get("request") or {}
            pending_models[stream] = {"index": len(report["requests"]) + 1, "kind": request.get("kind", "turn"),
                                      "stream": stream, "started": iso(at), "input_bytes": request.get("input_bytes")}
            report["requests"].append(pending_models[stream])
        elif kind == "model_call_end":
            request = event.get("request") or {}
            call = pending_models.pop(stream, None)
            if call is None:
                report["warnings"].append("model_end_without_start")
                call = {"index": len(report["requests"]) + 1, "stream": stream, "kind": request.get("kind", "turn")}
                report["requests"].append(call)
            call.update({"finished": iso(at), "duration_ms": request.get("duration_ms", 0),
                         "failed": request.get("failed", False), "usage": request.get("usage"), "completed_observation": True})
            model_intervals.append((stamp(call.get("started")), at))
        elif kind == "tool_start":
            key = (stream, event.get("tool_id"))
            if key in pending_tools:
                report["warnings"].append("unpaired_tool_start")
            tool = {"tool_id": event.get("tool_id"), "name": event.get("tool_name"), "stream": stream,
                    "started": iso(at), **tool_inputs.get(event.get("tool_id"), {})}
            pending_tools[key] = tool
            report["tools"].append(tool)
        elif kind == "decision_operation":
            try:
                operation = json.loads(event.get("text", "{}"))
            except json.JSONDecodeError:
                report["warnings"].append("malformed_decision_operation")
                continue
            selected = {key: operation.get(key) for key in ("op", "elapsed_ms", "failed", "state_changed", "committed", "actions")}
            selected["event_index"] = index
            report["decision_operations"].append(selected)
            candidates = [tool for (which, _), tool in pending_tools.items() if which == stream and tool.get("name") in {"graph_action", "read_graph", "read_snapshot"}]
            if len(candidates) == 1:
                candidates[0].setdefault("decision_operations", []).append(selected)
        elif kind == "tool_end":
            key = (stream, event.get("tool_id"))
            tool = pending_tools.pop(key, None)
            if tool is None:
                report["warnings"].append("tool_end_without_start")
                tool = {"tool_id": event.get("tool_id"), "name": event.get("tool_name"), "stream": stream, **tool_inputs.get(event.get("tool_id"), {})}
                report["tools"].append(tool)
            begin = stamp(tool.get("started"))
            operations = tool.get("decision_operations", [])
            tool.update({"finished": iso(at), "duration_ms": (at - begin) * 1000 if at is not None and begin is not None else None,
                         "failed": bool(event.get("error")), "error_kind": error_kind(event.get("error")),
                         "state_changed": error_kind(event.get("error")) == "state_changed" or any(op.get("state_changed") for op in operations)})
            tool_intervals.append((begin, at))
    # Do not manufacture an end timestamp for an interrupted request/tool.
    report["incomplete_model_observations"] = len(pending_models)
    report["incomplete_tool_observations"] = len(pending_tools)
    if pending_models or pending_tools:
        report["warnings"].append("incomplete_intervals_excluded_from_timeline")
    report["model_calls"] = len(report["requests"])
    report["summary_calls"] = sum(call["kind"] == "summary" for call in report["requests"])
    report["failed_model_calls"] = sum(bool(call.get("failed")) for call in report["requests"])
    report["model_duration_ms"] = stats(call.get("duration_ms") for call in report["requests"])
    report["tool_duration_ms"] = stats(tool.get("duration_ms") for tool in report["tools"])
    report.update(usage_totals(report["requests"]))
    for tool in report["tools"]:
        if not tool.get("state_changed"):
            continue
        conflict_at = stamp(tool.get("finished"))
        later = [call["index"] for call in report["requests"] if conflict_at is not None and stamp(call.get("started")) is not None and stamp(call["started"]) > conflict_at]
        tool["model_requests_after_conflict"] = later
    return report, model_intervals, tool_intervals


def analyze(output):
    manifest = read_json(output / "manifest.json", {})
    observed = read_json(output / "runs.json", []) or []
    state_events = read_json(output / "state-events.json", []) or []
    proxy = read_json(output / "http-observations.json", []) or []
    validation = read_json(output / "validation.json", {})
    started = stamp(manifest.get("started"))
    completions = [stamp(event.get("created_at")) for event in state_events if event.get("op") == "complete"]
    completions = [value for value in completions if value is not None]
    finished = min(completions) if completions else stamp(manifest.get("completed_observed"))
    end_basis = "authoritative_complete_state_event" if completions else "completed_observed_poll" if finished else "last_observed_run_finish_incomplete_project"
    if finished is None:
        candidates = [stamp(run.get("finished")) for run in observed]
        finished = max((value for value in candidates if value is not None), default=None)
    if started is None or finished is None or finished <= started:
        raise ValueError("retained evidence lacks a valid project time window")
    run_dir = output / "workspace" / ".xloom" / "runs"
    observations = {run["run_id"]: run for run in observed}
    all_ids = set(observations) | {directory.name for directory in run_dir.iterdir() if directory.is_dir()} if run_dir.exists() else set(observations)
    runs, model_intervals, tool_intervals = [], [], []
    for run_id in sorted(all_ids, key=lambda key: observations.get(key, {}).get("started", "")):
        run, models, tools = analyze_run(run_dir / run_id, observations.get(run_id, {}))
        runs.append(run)
        model_intervals.extend(models)
        tool_intervals.extend(tools)
    requests = [{"run_id": run["run_id"], "run_kind": run["kind"], **call} for run in runs for call in run["requests"]]
    tool_calls = [{"run_id": run["run_id"], **call} for run in runs for call in run["tools"]]
    conflicts = [call for call in tool_calls if call.get("state_changed")]
    external_updates = []
    for run in runs:
        if run["kind"] != "reason":
            continue
        begin, end = stamp(run["started"]), stamp(run["finished"])
        if begin is None or end is None:
            continue
        for event in state_events:
            at = stamp(event.get("created_at"))
            if event.get("op") in BUSINESS_OPS and event.get("run_id") and event["run_id"] != run["run_id"] and at is not None and begin <= at <= end:
                external_updates.append({"decision_run_id": run["run_id"], **{key: event.get(key) for key in ("revision", "op", "id", "run_id", "created_at")}})
    attempts = []
    for item in proxy:
        attempt = {key: item.get(key) for key in ("request_id", "run_id", "model", "max_tokens", "tool_count", "started_at", "finished_at", "http_status", "headers_ms", "first_event_ms", "first_thinking_ms", "last_thinking_ms", "first_output_ms", "last_output_ms", "thinking_chars", "output_chars", "stop_reason", "errors")}
        start, end = stamp(item.get("started_at")), stamp(item.get("finished_at"))
        attempt["duration_ms"] = (end - start) * 1000 if start is not None and end is not None else None
        first_content = [item[key] for key in ("first_thinking_ms", "first_output_ms") if item.get(key) is not None]
        attempt["first_content_ms"] = min(first_content) if first_content else None
        for label in ("thinking", "output"):
            first, last = item.get(f"first_{label}_ms"), item.get(f"last_{label}_ms")
            attempt[f"{label}_span_ms"] = max(0, last - first) if first is not None and last is not None else None
        matching = [call for call in requests if call["run_id"] == item.get("run_id") and stamp(call.get("started")) is not None and start is not None and stamp(call["started"]) <= start and (stamp(call.get("finished")) is None or start <= stamp(call["finished"]))]
        attempt["logical_request_index"] = matching[0]["index"] if len(matching) == 1 else None
        attempts.append(attempt)
    counts = Counter((a["run_id"], a["logical_request_index"]) for a in attempts if a["logical_request_index"] is not None)
    execute_intervals = [(stamp(run["started"]), stamp(run["finished"])) for run in runs if run["kind"] in {"explore", "bootstrap"}]
    longest = sorted((call for call in requests if call.get("duration_ms") is not None), key=lambda call: call["duration_ms"], reverse=True)[:10]
    budget_failures = [run["run_id"] for run in runs if run.get("failure_kind") == "budget_exhausted"]
    read_conflicts = [tool for tool in conflicts if tool["name"] in {"read_graph", "read_snapshot"}]
    terminal_conflicts = [tool for tool in conflicts if tool.get("op") in {"preview", "commit"} or any(op.get("op") in {"decision_preview", "decision_commit"} for op in tool.get("decision_operations", []))]
    report = {"version": 1, "project_id": manifest.get("project_id"), "model": manifest.get("model"),
              "source_commit": manifest.get("source_commit"), "reasoning_effort": manifest.get("reasoning_effort"),
              "started": iso(started), "finished": iso(finished), "end_basis": end_basis,
              "project_wall_seconds": finished - started,
              "poll_completion_wall_seconds": manifest.get("project_wall_seconds"),
              "business_validation": validation, "runs": runs,
              "timing": timeline(model_intervals, tool_intervals, (started, finished)),
              "cumulative_model_duration_ms": sum(call.get("duration_ms", 0) for call in requests),
              "cumulative_tool_duration_ms": sum(tool.get("duration_ms") or 0 for tool in tool_calls),
              "model_duration_ms": stats(call.get("duration_ms") for call in requests),
              "tool_duration_ms": stats(tool.get("duration_ms") for tool in tool_calls),
              "tool_duration_by_name_ms": {name: stats(tool.get("duration_ms") for tool in tool_calls if tool.get("name") == name) for name in sorted({tool.get("name", "unknown") for tool in tool_calls})},
              "model_calls": len(requests), "summary_calls": sum(call["kind"] == "summary" for call in requests),
              "failed_model_calls": sum(bool(call.get("failed")) for call in requests),
              "tool_calls": len(tool_calls), "longest_model_calls": longest,
              "http_attempts": attempts, "http_attempt_count": len(attempts),
              "http_retries_in_matched_logical_calls": sum(max(0, count - 1) for count in counts.values()),
              "unmatched_http_attempts": sum(a["logical_request_index"] is None for a in attempts),
              "http_timing_ms": {key: stats(a.get(key) for a in attempts) for key in ("duration_ms", "headers_ms", "first_event_ms", "first_content_ms", "first_thinking_ms", "first_output_ms", "thinking_span_ms", "output_span_ms")},
              "thinking_chars": sum(a.get("thinking_chars") or 0 for a in attempts),
              "output_chars": sum(a.get("output_chars") or 0 for a in attempts),
              "cost_status": "unknown_no_verified_account_pricing_or_bill", "cost_amount": None,
              "conflicts": conflicts, "external_business_updates_during_decisions": external_updates,
              "coverage": {"peak_concurrent_execute_runs": peak_concurrency(execute_intervals),
                           "all_run_evidence_present": bool(runs) and not any(run["missing_evidence_files"] for run in runs),
                           "at_least_two_execute_overlap": peak_concurrency(execute_intervals) >= 2,
                           "decision_saw_external_business_updates": bool(external_updates),
                           "decision_saw_external_fact_updates": any(event["op"] in {"fact", "step_completed"} for event in external_updates),
                           "state_changed_observed": bool(conflicts), "state_changed_count": len(conflicts),
                           "read_state_changed_count": len(read_conflicts), "no_read_state_changed": not read_conflicts,
                           "preview_commit_conflicts": len(terminal_conflicts),
                           "model_calls_after_preview_commit_conflict": sum(len(tool["model_requests_after_conflict"]) for tool in terminal_conflicts),
                           "no_budget_exhaustion": not budget_failures, "budget_exhausted_runs": budget_failures,
                           "incomplete_model_observations": sum(run["incomplete_model_observations"] for run in runs),
                           "incomplete_tool_observations": sum(run["incomplete_tool_observations"] for run in runs)},
              **usage_totals(requests)}
    report["run_kind_totals"] = {kind: {"runs": sum(run["kind"] == kind for run in runs),
                                        "model_calls": sum(call["run_kind"] == kind for call in requests),
                                        "model_duration_ms": sum(call.get("duration_ms", 0) for call in requests if call["run_kind"] == kind),
                                        **usage_totals([call for call in requests if call["run_kind"] == kind])}
                                 for kind in sorted({run["kind"] for run in runs if run["kind"]})}
    return report


def render(report):
    timing, coverage, usage = report["timing"], report["coverage"], report["reported_usage"]
    lines = ["# X-Loom 真实模型并发验收与耗时", "",
             f"模型 `{report['model']}`，reasoning=`{report['reasoning_effort']}`，源码 `{report['source_commit']}`。项目 `{report['project_id']}`。",
             f"业务验收：{'通过' if report['business_validation'].get('passed') else '未通过或尚未提供'}。项目墙钟 **{report['project_wall_seconds']:.3f} 秒**（结束依据：{report['end_basis']}）。",
             "", "## 时间归因", "", "| 项目墙钟内活动 | 秒 | 占项目墙钟 |", "| --- | ---: | ---: |"]
    for label, key in (("仅模型请求", "model_only_seconds"), ("仅工具执行", "tool_only_seconds"), ("模型与工具并发", "overlap_seconds"), ("两者均未活动", "neither_seconds")):
        lines.append(f"| {label} | {timing[key]:.3f} | {timing[key] / timing['wall_seconds']:.1%} |")
    lines += ["", f"模型请求累计 {report['cumulative_model_duration_ms'] / 1000:.3f} 秒，工具累计 {report['cumulative_tool_duration_ms'] / 1000:.3f} 秒。累计值包含并发，不能直接除以项目墙钟作为占比。空档包含调度、容器、落盘、图更新及尚未观测的工作，不等同于纯调度开销。",
              f"逻辑模型请求 {report['model_calls']} 次（summary {report['summary_calls']}，失败 {report['failed_model_calls']}）；HTTP 尝试 {report['http_attempt_count']} 次，已匹配逻辑调用内重试 {report['http_retries_in_matched_logical_calls']} 次；工具 {report['tool_calls']} 次。",
              "", "| 请求耗时统计 | p50 秒 | p95 秒 | 最大秒 |", "| --- | ---: | ---: | ---: |"]
    for label, data in (("模型逻辑请求", report["model_duration_ms"]), ("工具", report["tool_duration_ms"])):
        lines.append(f"| {label} | {data.get('p50', 0) / 1000:.3f} | {data.get('p95', 0) / 1000:.3f} | {data.get('max', 0) / 1000:.3f} |")
    lines += ["", "百分位采用 nearest rank。", "", "## 各 run", "", "| run | 种类 / 状态 | 输入 revision | 墙钟秒 | 模型次数 / 累计秒 | 工具次数 / 累计秒 |", "| --- | --- | ---: | ---: | ---: | ---: |"]
    for run in report["runs"]:
        lines.append(f"| `{run['run_id']}` | {run['kind']} / {run['status']} | {run['input_revision']} | {(run['wall_seconds'] or 0):.3f} | {run['model_calls']} / {run['model_duration_ms'].get('sum', 0) / 1000:.3f} | {len(run['tools'])} / {run['tool_duration_ms'].get('sum', 0) / 1000:.3f} |")
    lines += ["", "完整输入版本、每次调用和最长请求见 timing-report.json。", "", "## 模型侧观测", "", "| HTTP / SSE 指标 | 有数据次数 | p50 秒 | p95 秒 | 最大秒 |", "| --- | ---: | ---: | ---: | ---: |"]
    labels = {"headers_ms": "到响应头", "first_event_ms": "到首 SSE 事件", "first_content_ms": "到首内容块（TTFT 近似）", "first_output_ms": "到首文本/工具输出块", "thinking_span_ms": "thinking 首末块跨度", "output_span_ms": "输出首末块跨度"}
    for key, label in labels.items():
        data = report["http_timing_ms"][key]
        lines.append(f"| {label} | {data['count']} | {data.get('p50', 0) / 1000:.3f} | {data.get('p95', 0) / 1000:.3f} | {data.get('max', 0) / 1000:.3f} |")
    lines += ["", "这些时间由透明代理读取真实响应字节时记录；首内容块可为空，所以 TTFT 是近似值。thinking 跨度包含流传输与服务端间隔，不是纯 GPU 思考时间；请求前等待也无法分离网络、排队和内部计算。thinking_chars 是字符数，不是 token。报告不保留思考正文。",
              f"观测 thinking 字符 {report['thinking_chars']}，输出字符 {report['output_chars']}。",
              "", "## 版本冲突与覆盖", "",
              f"Execute 峰值并发 {coverage['peak_concurrent_execute_runs']}；Decide 运行期间其他 run 的业务更新 {len(report['external_business_updates_during_decisions'])} 条。",
              f"state_changed {coverage['state_changed_count']} 次；读图冲突 {coverage['read_state_changed_count']} 次；preview/commit 冲突 {coverage['preview_commit_conflicts']} 次；这些冲突后旧 run 新模型请求 {coverage['model_calls_after_preview_commit_conflict']} 次。",
              f"预算耗尽 run：{len(coverage['budget_exhausted_runs'])}；未闭合模型/工具观测：{coverage['incomplete_model_observations']} / {coverage['incomplete_tool_observations']}（不完整区间未计入活动并集）。"]
    if not coverage["state_changed_observed"]:
        lines.append("本次没有触发版本冲突，不能声称真实模型已经覆盖冲突退出路径；并发业务更新是否覆盖见上述记录。")
    if not coverage["at_least_two_execute_overlap"] or not coverage["decision_saw_external_fact_updates"]:
        lines.append("本次并发覆盖不足：未同时满足至少两个 Execute 重叠及 Decide 期间其他 run 的事实更新。")
    if not coverage["all_run_evidence_present"]:
        lines.append("部分 run 原始证据文件缺失；计时、用量及未发现故障结论均不完整。")
    lines += ["", "## 用量与费用", "", f"provider 返回的输入 {usage['input_tokens']}、输出 {usage['output_tokens']}、缓存读取 {usage['cache_read_input_tokens']}、缓存写入 {usage['cache_creation_input_tokens']} token。覆盖状态 `{report['usage_status']}`，有 usage 的逻辑请求 {report['usage_calls']} 次。",
              "只累计 model_call_end 的 usage；message_end、摘要记录及代理 HTTP usage 不再重复相加。未返回的 usage、内部失败尝试账单无法据此确认。没有已核实的账号价格和账单，实际金额为未知。", "",
              "业务验收明细：`validation.json`；逐次计时：`timing-report.json`；原始证据：`runs.json`、`http-observations.json`、`state-events.json` 和 `workspace/.xloom/runs/`。", ""]
    return "\n".join(lines)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", required=True, type=Path)
    args = parser.parse_args()
    report = analyze(args.output)
    (args.output / "timing-report.json").write_text(json.dumps(report, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    (args.output / "report.md").write_text(render(report), encoding="utf-8")
    print(json.dumps({"report": str(args.output / "report.md"), "project_wall_seconds": report["project_wall_seconds"], "model_calls": report["model_calls"], "coverage": report["coverage"]}, ensure_ascii=False))


if __name__ == "__main__":
    main()
