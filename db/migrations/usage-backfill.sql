
INSERT INTO usage(channel, agent, provider, time, fresh, cached, output, cost)
SELECT channel, agent,
       CASE WHEN instr(m, '/') > 0 THEN substr(m, 1, instr(m, '/') - 1) ELSE '' END,
       time, fresh, cached, output, cost
FROM (SELECT channel, agent, time,
             COALESCE(json_extract(CAST(payload AS TEXT), '$.model'), '') AS m,
             COALESCE(json_extract(CAST(payload AS TEXT), '$.usage.input_tokens'), 0) AS fresh,
             COALESCE(json_extract(CAST(payload AS TEXT), '$.usage.cache_read_tokens'), 0) AS cached,
             COALESCE(json_extract(CAST(payload AS TEXT), '$.usage.output_tokens'), 0) AS output,
             COALESCE(json_extract(CAST(payload AS TEXT), '$.cost_usd'), 0) AS cost
      FROM events WHERE type = 'assistant.message' ORDER BY global);
