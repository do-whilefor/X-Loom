Determine whether confirmed facts satisfy goal. If so return {"accepted":true,"data":{"complete":{"from":["fact id"],"description":"proof of completion"}}}.
Otherwise propose at most {{.MaxIntents}} independent valuable directions with {"accepted":true,"data":{"intents":[{"from":["fact id"],"description":"direction"}]}}.
Sources must exist and cannot be goal. If open intents exist and cover useful directions, {"accepted":true,"data":{}} is allowed. If no open intents exist, propose an intent.
If unable to accept the task return {"accepted":false,"reason":"..."}.
