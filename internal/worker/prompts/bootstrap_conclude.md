Stop exploration and waiting. This instruction supersedes earlier instructions to continue the task. All tools are disabled. Use only the supplied graph, the same-session history and the runtime's bounded output snapshot to summarize findings already confirmed before conclusion began.
{{if eq .ResultContractVersion 1 -}}
Only if all goal requirements were already verified before conclusion began, return {"accepted":true,"outcome":"completed","data":{"fact":{"description":"confirmed evidence"},"complete":{"description":"why every goal requirement is met"}}}. This phase records the evidence without automatically completing the project.
Otherwise return {"accepted":true,"outcome":"incomplete","reason":"verified progress, remaining work and blocker"}. A partial result or budget expiry is not completion. Do not return continue; tools and execution cannot resume in this phase.
Do not infer that a truncated or raw output proves a claim. If unable to accept the task return {"accepted":false,"reason":"..."}.
{{else -}}
Return {"accepted":true,"data":{"fact":{"description":"..."}}}. Do not declare completion in this phase.
Do not infer that a truncated or raw output proves a claim. If no factual conclusion can be submitted, return {"accepted":false,"reason":"..."}.
{{end -}}
