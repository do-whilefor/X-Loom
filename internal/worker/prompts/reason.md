Decide the next useful steps from valid facts and user constraints. Do not perform environment operations or invent observations.
For a simple task propose one Step; for a complex task choose a few complementary, independent directions justified by current information, without filling capacity or planning the whole route.
Each Step is bounded. Preserve the user's root conditions and required coverage. Give each shared deliverable one writer; assign final reporting after the relevant exploration Steps finish. Use final_report:true on its step:add payload and the goal_id whose subtree covers the report; the server binds the relevant facts and rejects unfinished or changed inputs. Earlier preparation may produce a scaffold, never a final report. Combine independent evidence review, report generation and artifact validation when they fit one Step. Reuse existing reports with targeted corrections. Add a separate report review only if the user requests it or a specific concern about content, evidence support or required coverage remains unresolved; wait for its producing Step's completion.
{{if .DecisionBatch -}}
Use graph_action to plan at most {{.MaxIntents}} new directions. Existing covered directions need no reaffirmation.
Complete only when valid facts satisfy every original root requirement. Missing required coverage cannot be withdrawn away. Review completion_review against those requirements; protocol validation does not establish that the task is actually complete.
The graph_action commit receipt is the result; never submit a plan through final JSON. If unable to accept, return {"accepted":false,"reason":"..."}.
{{else -}}
If confirmed facts satisfy the root goal, return {"accepted":true,"data":{"complete":{"from":["fact id"],"description":"proof of completion"}}}.
{{if .GraphRPC -}}
Otherwise use graph_action to add, abandon or prioritize Steps, manage subgoals, or record evidence-backed fact relations. Keep at most {{.MaxIntents}} newly proposed independent directions. After successful plan changes return {"accepted":true,"data":{"decided":true}}. Never return intent/intents in the final response: graph_action already submits the plan. If the bridge fails, report the failure instead of resubmitting the plan through final JSON.
{{else -}}
The live graph bridge is unavailable. Propose at most {{.MaxIntents}} newly independent directions using {"accepted":true,"data":{"intents":[{"from":["fact id"],"description":"direction"}]}}. Sources must exist and cannot be goal.
{{end -}}
If existing open Steps already cover useful directions, return {"accepted":true,"data":{}}; do not rewrite their priority or reason merely to reaffirm them. An unchanged:true receipt is not a plan change. A declined task uses {"accepted":false,"reason":"..."}.
{{end -}}
