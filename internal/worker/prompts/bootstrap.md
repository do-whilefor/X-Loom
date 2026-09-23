Work directly from origin toward goal. Continue until the goal is confirmed or a conclude instruction arrives.
{{if eq .ResultContractVersion 2 -}}
Only after all goal requirements are verified, return {"accepted":true,"outcome":"completed","data":{"fact_id":"ID of a published evidence-backed fact from this Step","complete":{"description":"why every goal requirement is met"}}}.
Alternatively return {"accepted":true,"outcome":"completed","data":{"fact":{"description":"confirmed evidence","scope":"conditions and target checked","observed_at":"2026-01-01T00:00:00Z","evidence":[{"path":"existing evidence file"}]},"complete":{"description":"why every goal requirement is met"}}} using the actual observation time. Omit run_id and excerpt; the runtime freezes exact bytes.
If work remains possible, return {"accepted":true,"outcome":"continue","reason":"remaining work"} to continue under the original budget.
If unable to finish, return {"accepted":true,"outcome":"incomplete","reason":"verified progress, remaining work and blocker"}. Preserve important observations through graph_action before stopping.
{{else if eq .ResultContractVersion 1 -}}
Only after all goal requirements are verified, return {"accepted":true,"outcome":"completed","data":{"fact":{"description":"confirmed evidence"},"complete":{"description":"why every goal requirement is met"}}}.
Partial findings, a progress summary, or intended next actions are not completion. Keep working; if ending a turn with work still possible, return {"accepted":true,"outcome":"continue","reason":"remaining work"} to resume this execution with its original budget.
If unable to finish, return {"accepted":true,"outcome":"incomplete","reason":"verified progress, remaining work and blocker"}. Before stopping, preserve important verified evidence through graph_action.
{{else -}}
On success return {"accepted":true,"data":{"fact":{"description":"confirmed evidence"},"complete":{"description":"why goal is met"}}}.
{{end -}}
If unable to accept the task return {"accepted":false,"reason":"..."}.
