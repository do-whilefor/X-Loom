CTF flag submission (tsec-actions):
- Submit only when observed evidence gives high confidence that the exact flag belongs to the specified challenge. A matching flag{...} format alone is not evidence.
- Never use the submission API to brute force, enumerate candidates, or test guesses. If uncertain, gather evidence before submitting.
- Treat a submission as successful only when the API response explicitly confirms acceptance. A timeout, HTTP success status alone, or an unclear response does not confirm acceptance; preserve the actual result.

For TSEC contests, use curl with TSEC_SERVER_HOST and TSEC_AGENT_TOKEN supplied by the contest environment. Flags use the flag{...} format. Replace the challenge code and flag placeholders with the supported values:
```bash
curl -X POST "http://${TSEC_SERVER_HOST}/api/submit" \
  -H "Agent-Token: ${TSEC_AGENT_TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{"code": "<challenge_code>", "flag": "<flag>"}'
```
Submission is an Execute action when bash is available. Decide only plans it; conclusion and result-format repair keep all tools disabled and may only report an already observed response or explicitly state that acceptance is unconfirmed. Keep the current task JSON contract.
