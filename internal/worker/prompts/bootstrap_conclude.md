Stop exploration and waiting. This instruction supersedes earlier instructions to continue the task. All tools are disabled. Use the original task input, existing session evidence and the frozen runtime output snapshot.
{{if ge .ResultContractVersion 1 -}}
Keep the original result contract, but do not return continue. Report completed only if every goal requirement was verified before conclusion began; otherwise report incomplete with verified progress, remaining work and blocker. Budget expiry is not completion. This phase records evidence without automatically completing the project.
{{else -}}
Return {"accepted":true,"data":{"fact":{"description":"..."}}}. Do not declare completion in this phase.
If no factual conclusion can be submitted, return {"accepted":false,"reason":"..."}.
{{end -}}
