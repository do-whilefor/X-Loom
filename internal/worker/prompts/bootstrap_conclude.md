Stop exploration and waiting. This instruction supersedes earlier instructions to continue the task. All tools are disabled. Use only the supplied graph, the same-session history and the runtime's bounded output snapshot to summarize findings already confirmed before conclusion began.
{{if eq .ResultContractVersion 2 -}}
Only if all goal requirements were already verified, return {"accepted":true,"outcome":"completed","data":{"fact_id":"ID of a published evidence-backed fact from this Step","complete":{"description":"why every goal requirement is met"}}}. This phase records evidence without automatically completing the project.
Alternatively return {"accepted":true,"outcome":"completed","data":{"fact":{"description":"already confirmed evidence","scope":"conditions and target checked","observed_at":"2026-01-01T00:00:00Z","evidence":[{"path":"evidence.path from the frozen runtime snapshot"}]},"complete":{"description":"why every goal requirement is met"}}} using the actual observation time. Only frozen fragments can supply new final evidence; omit run_id and excerpt.
Otherwise return {"accepted":true,"outcome":"incomplete","reason":"verified progress, remaining work and blocker"}. Do not return continue. If unable to accept the task return {"accepted":false,"reason":"..."}.
{{else if eq .ResultContractVersion 1 -}}
Only if all goal requirements were already verified before conclusion began, return {"accepted":true,"outcome":"completed","data":{"fact":{"description":"confirmed evidence"},"complete":{"description":"why every goal requirement is met"}}}. This phase records the evidence without automatically completing the project.
Otherwise return {"accepted":true,"outcome":"incomplete","reason":"verified progress, remaining work and blocker"}. A partial result or budget expiry is not completion. Do not return continue; tools and execution cannot resume in this phase.
Do not infer that a truncated or raw output proves a claim. If unable to accept the task return {"accepted":false,"reason":"..."}.
{{else -}}
Return {"accepted":true,"data":{"fact":{"description":"..."}}}. Do not declare completion in this phase.
Do not infer that a truncated or raw output proves a claim. If no factual conclusion can be submitted, return {"accepted":false,"reason":"..."}.
{{end -}}
