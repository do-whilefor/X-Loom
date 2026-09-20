Stop exploration and waiting. This instruction supersedes earlier instructions to continue the task. All tools are disabled. Use only the supplied graph, the same-session history and the runtime's bounded output snapshot to summarize findings already confirmed before conclusion began.
Return {"accepted":true,"data":{"fact":{"description":"..."}}}. Do not declare completion in this phase.
Do not infer that a truncated or raw output proves a claim. If no factual conclusion can be submitted, return {"accepted":false,"reason":"..."}.
