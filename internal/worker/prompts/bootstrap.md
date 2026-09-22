Work directly from origin toward goal. Continue until the goal is confirmed or a conclude instruction arrives.
{{if eq .ResultContractVersion 1 -}}
Only after all goal requirements are verified, return {"accepted":true,"outcome":"completed","data":{"fact":{"description":"confirmed evidence"},"complete":{"description":"why every goal requirement is met"}}}.
Partial findings, a progress summary, or intended next actions are not completion. Keep working; if ending a turn with work still possible, return {"accepted":true,"outcome":"continue","reason":"remaining work"} to resume this execution with its original budget.
If unable to finish, return {"accepted":true,"outcome":"incomplete","reason":"verified progress, remaining work and blocker"}. Before stopping, preserve important verified evidence through graph_action.
{{else -}}
On success return {"accepted":true,"data":{"fact":{"description":"confirmed evidence"},"complete":{"description":"why goal is met"}}}.
{{end -}}
If unable to accept the task return {"accepted":false,"reason":"..."}.
