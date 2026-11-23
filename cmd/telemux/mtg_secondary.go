package main

import (
	"errors"
	"net"
	"net/url"
	"regexp"
	"strings"
)

// Неиспользуемый второй экземпляр выключен. Его настройки берутся только из локального env/CLI.
func validateSecondaryMtg(file, unit, stats, mainUnit, mainStats string) error {
	if file == "" && unit == "" && stats == "" {
		return nil
	}
	if file == "" || unit == "" || stats == "" {
		return errors.New("для второго mtg задайте файл, юнит и адрес статистики вместе")
	}
	if !mtgUnitName.MatchString(unit) || strings.TrimSuffix(unit, ".service") == strings.TrimSuffix(mainUnit, ".service") {
		return errors.New("второму mtg нужен отдельный юнит systemd")
	}
	second, err := url.Parse(stats)
	if err != nil || second.Scheme != "http" || second.User != nil || second.Port() == "" || second.Path != "/stats" ||
		second.RawQuery != "" || second.Fragment != "" || (second.Hostname() != "localhost" && !net.ParseIP(second.Hostname()).IsLoopback()) {
		return errors.New("статистика второго mtg должна указывать на локальный HTTP /stats с явным портом")
	}
	first, err := url.Parse(mainStats)
	if err != nil || first.Port() == second.Port() {
		return errors.New("для второго mtg нужен отдельный порт статистики")
	}
	return nil
}

var mtgUnitName = regexp.MustCompile(`^[[:alnum:]][[:alnum:]_.@-]*$`)
