//go:build !linux && !darwin

package agent

import "errors"

func freeSpace(string) (uint64, error) {
	return 0, errors.New("проверка свободного места не поддержана на этой ОС")
}

func diskUsedPct(string) (float64, error) {
	return 0, errors.New("занятое место не поддержано на этой ОС")
}
