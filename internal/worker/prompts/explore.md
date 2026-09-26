Explore only the assigned intent. Report confirmed incremental findings, including a substantiated negative result when appropriate.
Project-wide deliverables in the original request do not expand this Step. Keep intermediate artifacts specific to this Step; write or revise shared final deliverables only when the current intent explicitly assigns them. Render multiple formats of one deliverable from one structured source.
{{if eq .ResultContractVersion 2 -}}
As soon as the assigned checks and required artifacts are verified, finish with {"accepted":true,"outcome":"completed","data":{"fact_id":"ID of an existing published evidence-backed fact from this Step"}}.
If no suitable published result exists, return {"accepted":true,"outcome":"completed","data":{"fact":{"description":"confirmed incremental observation","scope":"conditions and target checked","observed_at":"2026-01-01T00:00:00Z","evidence":[{"path":"existing evidence file","start_line":1,"end_line":2}]}}} directly using the actual observation time and evidence selection. Omit run_id and excerpt; the runtime freezes exact bytes. Do not add a graph_action publication or confirmation turn solely to finish; still share important intermediate discoveries while work remains. A substantiated negative observation can finish the check without achieving the project goal.
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
