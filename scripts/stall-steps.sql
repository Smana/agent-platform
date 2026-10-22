-- stall-steps.sql: every implementer run in the room log, its steps and the step of its first
-- commit (loops design L3), as CSV for scripts/stall-steps.sh. A step is a tool call the bridge
-- mirrored, as agent-run counts them; a commit is a call whose output shows git's
-- "[<branch> <sha>] <subject>" line, so a failed or quiet `git commit` is not one. Read-only.
COPY (
  WITH calls AS (
    SELECT run_id, payload->>'callId' AS call_id, coalesce(payload->'args'->>'command', '') AS command,
           row_number() OVER (PARTITION BY run_id ORDER BY seq) AS step
    FROM events
    WHERE type = 'tool_call' AND origin = 'harness' AND actor_role = 'implementer' AND run_id IS NOT NULL
  ), results AS (
    SELECT run_id, payload->>'callId' AS call_id, payload->>'output' AS output
    FROM events
    WHERE type = 'tool_result' AND origin = 'harness' AND actor_role = 'implementer' AND run_id IS NOT NULL
  )
  SELECT c.run_id, max(c.step) AS steps,
         min(c.step) FILTER (WHERE c.command ~ '\mgit\M.*\mcommit\M'
                               AND r.output ~ '(^|\n)\[[^]\n]+ [0-9a-f]{7,40}\] ') AS first_commit_step
  FROM calls c LEFT JOIN results r ON r.run_id = c.run_id AND r.call_id = c.call_id
  GROUP BY c.run_id
  ORDER BY c.run_id
) TO STDOUT WITH (FORMAT csv, HEADER);
