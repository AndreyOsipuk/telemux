package agent

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/AndreyOsipuk/telemux/internal/store"
)

const runtimeTelemtMetricsLimit = 32 << 20

func isTrafficPilotUser(key string) bool {
	return strings.HasPrefix(key, "free_") && len(key) > len("free_")
}

// Telemt печатает отдельные целочисленные серии по направлению и ключу.
var telemtOctetsLine = regexp.MustCompile(`^telemt_user_octets_(from|to)_client_total\{user="([^"]+)"\}[ \t]+([^ \t]+)[ \t]*$`)

var systemdInvocationID = regexp.MustCompile(`^[0-9a-f]{32}$`)

func (c *RuntimeCollector) telemtGeneration(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	out, err := c.Runner.Run(ctx, "systemctl", "show", "--property=InvocationID", "--value", "--", c.TelemtUnit)
	if err != nil {
		return "", fmt.Errorf("InvocationID юнита %q: %w", c.TelemtUnit, err)
	}
	id := strings.TrimSpace(string(out))
	if !systemdInvocationID.MatchString(id) || id == strings.Repeat("0", 32) {
		return "", fmt.Errorf("юнит %q не вернул действующий InvocationID", c.TelemtUnit)
	}
	return "systemd:" + id, nil
}

type telemtOctets struct {
	in, out *int64
}

func (c *RuntimeCollector) collectTelemtTraffic(ctx context.Context, generation string, configured []telemtAPIUser, users map[string]*store.RuntimeUser) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.TelemtMetricsURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	metrics, err := parseTelemtTraffic(resp.Body)
	if err != nil {
		return "", err
	}
	for _, user := range configured {
		if !isTrafficPilotUser(user.Username) || user.TotalOctets == nil || *user.TotalOctets != 0 {
			continue
		}
		if _, found := metrics[user.Username]; !found {
			zero := int64(0)
			metrics[user.Username] = telemtOctets{in: &zero, out: &zero}
		}
	}
	if len(metrics) == 0 {
		return "", nil
	}
	after, err := c.telemtGeneration(ctx)
	if err != nil {
		return "", err
	}
	if after != generation {
		return "", fmt.Errorf("юнит %q перезапустился во время чтения /metrics", c.TelemtUnit)
	}
	hasSample := false
	for key, sample := range metrics {
		if sample.in == nil || sample.out == nil {
			continue
		}
		user := users[key]
		if user == nil {
			user = &store.RuntimeUser{Key: key}
			users[key] = user
		}
		user.BytesIn, user.BytesOut, user.TrafficSample = *sample.in, *sample.out, true
		hasSample = true
	}
	if !hasSample {
		return "", nil
	}
	return generation, nil
}

func parseTelemtTraffic(body io.Reader) (map[string]telemtOctets, error) {
	byKey := make(map[string]telemtOctets)
	input := &io.LimitedReader{R: body, N: runtimeTelemtMetricsLimit + 1}
	scanner := bufio.NewScanner(input)
	for scanner.Scan() {
		sample := telemtOctetsLine.FindStringSubmatch(scanner.Text())
		if sample == nil || !isTrafficPilotUser(sample[2]) {
			continue
		}
		value, err := strconv.ParseUint(sample[3], 10, 63)
		if err != nil {
			return nil, fmt.Errorf("невалидный счётчик telemt: %w", err)
		}
		key := sample[2]
		pair := byKey[key]
		direction := &pair.out
		if sample[1] == "from" {
			direction = &pair.in
		}
		if *direction != nil {
			return nil, fmt.Errorf("повторная серия счётчика telemt для %q", key)
		}
		octets := int64(value)
		*direction = &octets
		byKey[key] = pair
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if input.N == 0 {
		return nil, fmt.Errorf("ответ /metrics превышает %d байт", runtimeTelemtMetricsLimit)
	}
	return byKey, nil
}
