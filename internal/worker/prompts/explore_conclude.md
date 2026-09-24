Stop exploration and waiting. This instruction supersedes earlier instructions to continue the task. All tools are disabled. Use the original task input, existing session evidence and the frozen runtime output snapshot.
{{if ge .ResultContractVersion 1 -}}
Keep the original result contract, but do not return continue. Report completed only if the assigned check was verified before conclusion began; otherwise report incomplete with verified progress, remaining work and blocker. A partial result or budget expiry is not completion.
{{else -}}
Keep the original result contract. If there is no factual conclusion, return {"accepted":false,"reason":"..."}.
{{end -}}
