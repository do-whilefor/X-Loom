Explore only the assigned intent. Report confirmed incremental findings, including a substantiated negative result when appropriate.
Project-wide deliverables in the original request do not expand this Step. Keep intermediate artifacts specific to this Step; write or revise shared final deliverables only when the current intent explicitly assigns them.
{{if eq .ResultContractVersion 2 -}}
Only after the assigned observation is verified, return {"accepted":true,"outcome":"completed","data":{"fact_id":"ID of a published evidence-backed fact from this Step"}}.
If no suitable published result exists, return {"accepted":true,"outcome":"completed","data":{"fact":{"description":"confirmed incremental observation","scope":"conditions and target checked","observed_at":"2026-01-01T00:00:00Z","evidence":[{"path":"existing evidence file","start_line":1,"end_line":2}]}}} using the actual observation time and evidence selection. Omit run_id and excerpt; the runtime freezes exact bytes. A substantiated negative observation can finish the check without achieving the project goal.
If more work can finish this Step, return {"accepted":true,"outcome":"continue","reason":"remaining work"} to continue under the original budget.
If unable to finish, return {"accepted":true,"outcome":"incomplete","reason":"verified progress, remaining work and blocker"}. Preserve important observations through graph_action before stopping.
{{else if eq .ResultContractVersion 1 -}}
Only after all requirements of the assigned intent are verified, return {"accepted":true,"outcome":"completed","data":{"description":"confirmed results and how they satisfy the entire intent"}}.
Partial findings, a progress summary, or intended next actions are not completion. Keep working; if ending a turn with work still possible, return {"accepted":true,"outcome":"continue","reason":"remaining work"} to resume this execution with its original budget.
If unable to finish, return {"accepted":true,"outcome":"incomplete","reason":"verified progress, remaining work and blocker"}. Before stopping, preserve important verified evidence through graph_action.
{{else -}}
Return {"accepted":true,"data":{"description":"..."}}.
{{end -}}
If unable to accept the task return {"accepted":false,"reason":"..."}.
