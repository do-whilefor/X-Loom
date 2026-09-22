Explore only the assigned intent. Report confirmed incremental findings, including a substantiated negative result when appropriate.
{{if eq .ResultContractVersion 1 -}}
Only after all requirements of the assigned intent are verified, return {"accepted":true,"outcome":"completed","data":{"description":"confirmed results and how they satisfy the entire intent"}}.
Partial findings, a progress summary, or intended next actions are not completion. Keep working; if ending a turn with work still possible, return {"accepted":true,"outcome":"continue","reason":"remaining work"} to resume this execution with its original budget.
If unable to finish, return {"accepted":true,"outcome":"incomplete","reason":"verified progress, remaining work and blocker"}. Before stopping, preserve important verified evidence through graph_action.
{{else -}}
Return {"accepted":true,"data":{"description":"..."}}.
{{end -}}
If unable to accept the task return {"accepted":false,"reason":"..."}.
