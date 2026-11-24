package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// recordTraffic вызывается под блокировкой строки agent_runtime этой машины.
// Более старые снимки и повторы не меняют ни базу сравнения, ни историю.
func recordTraffic(ctx context.Context, tx pgx.Tx, target, product, generation string, observedAt time.Time, users []RuntimeUser) error {
	var keys []string
	var bytesIn, bytesOut []int64
	for _, user := range users {
		if user.TrafficSample {
			keys = append(keys, user.Key)
			bytesIn = append(bytesIn, user.BytesIn)
			bytesOut = append(bytesOut, user.BytesOut)
		}
	}
	if len(keys) == 0 {
		return nil
	}
	// Уникальный free_<id> начинается с нуля, остальные ключи первым замером задают базу.
	// Смена поколения и снижение счётчика free_<id> после перевыдачи ключа начинают новую базу.
	_, err := tx.Exec(ctx, `
		WITH samples AS MATERIALIZED (
			SELECT u.user_key, u.bytes_in, u.bytes_out,
			       CASE WHEN c.user_key IS NULL AND u.user_key ~ '^free_[0-9]+$' THEN '-infinity'::timestamptz
			            WHEN c.user_key IS NULL OR c.generation <> $3
			                 OR u.bytes_in < c.bytes_in OR u.bytes_out < c.bytes_out
			            THEN $4 ELSE c.coverage_since END AS coverage_since,
			       CASE WHEN c.user_key IS NULL THEN
			                CASE WHEN u.user_key ~ '^free_[0-9]+$' THEN u.bytes_in ELSE 0 END
			            WHEN c.generation <> '' AND $3 <> '' AND c.generation <> $3 THEN u.bytes_in
			            WHEN u.bytes_in < c.bytes_in AND ($3 = '' OR u.user_key ~ '^free_[0-9]+$')
			                THEN u.bytes_in
			            ELSE GREATEST(u.bytes_in - c.bytes_in, 0) END AS delta_in,
			       CASE WHEN c.user_key IS NULL THEN
			                CASE WHEN u.user_key ~ '^free_[0-9]+$' THEN u.bytes_out ELSE 0 END
			            WHEN c.generation <> '' AND $3 <> '' AND c.generation <> $3 THEN u.bytes_out
			            WHEN u.bytes_out < c.bytes_out AND ($3 = '' OR u.user_key ~ '^free_[0-9]+$')
			                THEN u.bytes_out
			            ELSE GREATEST(u.bytes_out - c.bytes_out, 0) END AS delta_out
			  FROM unnest($5::text[], $6::bigint[], $7::bigint[]) AS u(user_key, bytes_in, bytes_out)
			  LEFT JOIN runtime_traffic_counters c
			    ON c.target = $1 AND c.product = $2 AND c.user_key = u.user_key
			 WHERE c.observed_at IS NULL OR c.observed_at < $4
		), updated AS (
			INSERT INTO runtime_traffic_counters (target, product, user_key, generation, coverage_since, observed_at, bytes_in, bytes_out)
			SELECT $1, $2, user_key, $3, coverage_since, $4, bytes_in, bytes_out FROM samples
			ON CONFLICT (target, product, user_key) DO UPDATE
			    SET generation = EXCLUDED.generation, coverage_since = EXCLUDED.coverage_since,
			        observed_at = EXCLUDED.observed_at,
			        bytes_in = EXCLUDED.bytes_in, bytes_out = EXCLUDED.bytes_out
			RETURNING user_key
		)
		INSERT INTO runtime_traffic_deltas (user_key, observed_at, target, product, bytes_in, bytes_out)
		SELECT s.user_key, $4, $1, $2, s.delta_in, s.delta_out
		  FROM samples s JOIN updated u USING (user_key)
		 WHERE s.delta_in > 0 OR s.delta_out > 0`,
		target, product, generation, observedAt, keys, bytesIn, bytesOut)
	if err != nil {
		return fmt.Errorf("сохранение трафика %s/%s: %w", target, product, err)
	}
	return nil
}

// TrafficUsage - подтверждённый расход по стабильному ключу.
type TrafficUsage struct {
	TotalBytes  json.Number `json:"total_bytes"`
	WindowBytes json.Number `json:"window_bytes"`
	MeasuredAt  *time.Time  `json:"measured_at"`
	Complete    bool        `json:"complete"`
}

// TrafficUsage считает оба интервала одним снимком базы. Дельта относится
// ко времени наблюдения. Полнота требует свежих данных всех включённых
// источников, включая второй mtg. Для free_<id> после сброса следующий
// устойчивый снимок восстанавливает полноту текущего измерения.
func (s *Store) TrafficUsage(ctx context.Context, key string, since, windowSince, until, freshSince time.Time) (TrafficUsage, error) {
	var usage TrafficUsage
	var total, window string
	err := s.pool.QueryRow(ctx, `
		WITH expected AS (
			SELECT t.code AS target, p.product
			  FROM targets t
			  CROSS JOIN (VALUES ($6::text, 'telemt'), ($7::text, 'mtg'), ($8::text, 'mtg_secondary'))
			             AS p(resource, product)
			 WHERE t.enabled AND p.resource = ANY(t.resources)
		), coverage AS (
			SELECT COALESCE(bool_and(
			        c.observed_at IS NOT NULL AND c.observed_at >= GREATEST($2::timestamptz, $5::timestamptz)
			        AND c.observed_at < $4 AND c.generation <> ''
			        AND (c.coverage_since <= $2 OR ($1 ~ '^free_[0-9]+$' AND c.coverage_since < c.observed_at))
			        AND COALESCE(CASE e.product
			            WHEN 'telemt' THEN a.telemt->'api_ok' = 'true'::jsonb
			            WHEN 'mtg' THEN a.mtg->'api_ok' = 'true'::jsonb
			            WHEN 'mtg_secondary' THEN a.mtg_secondary_api_ok
			        END, false)
			    ), false) AS complete
			  FROM expected e LEFT JOIN runtime_traffic_counters c
			    ON c.target = e.target AND c.product = e.product AND c.user_key = $1
			  LEFT JOIN agent_runtime a ON a.target = e.target
		)
		SELECT COALESCE(sum(bytes_in::numeric + bytes_out), 0)::text,
		       COALESCE(sum(bytes_in::numeric + bytes_out) FILTER (WHERE observed_at >= $3), 0)::text,
		       (SELECT max(observed_at) FROM runtime_traffic_counters
		         WHERE user_key = $1 AND observed_at >= $2 AND observed_at < $4),
		       (SELECT complete FROM coverage)
		  FROM runtime_traffic_deltas
		 WHERE user_key = $1 AND observed_at >= $2 AND observed_at < $4`,
		key, since, windowSince, until, freshSince, ResourceTelemtUsers, ResourceMtgConfig, ResourceMtgSecondaryConfig).
		Scan(&total, &window, &usage.MeasuredAt, &usage.Complete)
	if err != nil {
		return TrafficUsage{}, fmt.Errorf("чтение расхода трафика: %w", err)
	}
	usage.TotalBytes, usage.WindowBytes = json.Number(total), json.Number(window)
	if usage.MeasuredAt != nil {
		at := usage.MeasuredAt.UTC()
		usage.MeasuredAt = &at
	}
	return usage, nil
}
