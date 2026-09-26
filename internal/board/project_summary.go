package board

// ProjectSummaries reads the list projection without materializing graph
// descriptions, evidence or intent sources. Aggregate each table once rather
// than loading every project's graph separately. Counts retain Summarize's
// legacy meaning: concluded_at, not to_fact_id, determines open work.
func (t *Tx) ProjectSummaries() ([]Summary, error) {
	rows, err := t.Query(`WITH
fact_counts AS (SELECT project_id,COUNT(*) AS total FROM facts GROUP BY project_id),
intent_counts AS (
 SELECT project_id,COUNT(*) AS total,
 SUM(concluded_at IS NULL AND worker IS NOT NULL) AS working,
 SUM(concluded_at IS NULL AND worker IS NULL) AS unclaimed
 FROM intents GROUP BY project_id
),
hint_counts AS (SELECT project_id,COUNT(*) AS total FROM hints GROUP BY project_id)
SELECT p.id,p.title,p.status,p.bootstrap_enabled,p.created_at,
 p.reason_worker,p.reason_trigger,p.reason_started_at,p.reason_last_heartbeat_at,
 COALESCE(m.scenario,''),COALESCE(round.generation,0),COALESCE(round.restarted_at,''),COALESCE(termination.terminated_at,''),
 COALESCE(f.total,0),COALESCE(i.total,0),COALESCE(i.working,0),COALESCE(i.unclaimed,0),COALESCE(h.total,0)
FROM projects p
LEFT JOIN xloom_project_metadata m ON m.project_id=p.id
LEFT JOIN xloom_project_rounds round ON round.project_id=p.id
LEFT JOIN xloom_project_termination termination ON termination.project_id=p.id
LEFT JOIN fact_counts f ON f.project_id=p.id
LEFT JOIN intent_counts i ON i.project_id=p.id
LEFT JOIN hint_counts h ON h.project_id=p.id
ORDER BY p.created_at,p.rowid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Summary{}
	for rows.Next() {
		var summary Summary
		var worker, trigger, started, heartbeat *string
		if err := rows.Scan(&summary.ID, &summary.Title, &summary.Status, &summary.Bootstrap, &summary.CreatedAt,
			&worker, &trigger, &started, &heartbeat, &summary.Scenario, &summary.Generation, &summary.RestartedAt, &summary.TerminatedAt,
			&summary.FactCount, &summary.IntentCount, &summary.Working, &summary.Unclaimed, &summary.HintCount); err != nil {
			return nil, err
		}
		if worker != nil {
			summary.Reason = &Reason{*worker, Value(trigger), Value(started), Value(heartbeat)}
		}
		out = append(out, summary)
	}
	return out, rows.Err()
}
