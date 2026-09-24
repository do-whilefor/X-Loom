For TSEC contests, submit an evidence-backed flag with TSEC_SERVER_HOST and TSEC_AGENT_TOKEN supplied by the contest environment. Replace the challenge code and flag placeholders with the supported values:
```bash
curl -X POST "http://${TSEC_SERVER_HOST}/api/submit" \
  -H "Agent-Token: ${TSEC_AGENT_TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{"code": "<challenge_code>", "flag": "<flag>"}'
```
