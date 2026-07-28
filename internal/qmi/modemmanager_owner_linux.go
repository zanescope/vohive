package qmicore

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const maxModemManagerAncestorDepth = 16

type modemManagerOwnership struct {
	Owned  bool
	Reason string
	Cgroup string
}

func detectModemManagerOwnershipAt(procRoot string, pid int) modemManagerOwnership {
	procRoot = strings.TrimSpace(procRoot)
	if procRoot == "" || pid <= 0 {
		return modemManagerOwnership{}
	}

	holderCgroup := readProcessCgroupAt(procRoot, pid)
	currentPID := pid
	seen := make(map[int]struct{}, maxModemManagerAncestorDepth)
	for depth := 0; depth < maxModemManagerAncestorDepth && currentPID > 0; depth++ {
		if _, ok := seen[currentPID]; ok {
			break
		}
		seen[currentPID] = struct{}{}

		cgroup := readProcessCgroupAt(procRoot, currentPID)
		if modemManagerServiceCgroup(cgroup) {
			reason := "holder_cgroup"
			if depth > 0 {
				reason = "ancestor_cgroup"
			}
			return modemManagerOwnership{Owned: true, Reason: reason, Cgroup: holderCgroup}
		}
		if strings.EqualFold(readProcessNameAt(procRoot, currentPID), "ModemManager") {
			reason := "holder_process"
			if depth > 0 {
				reason = "ancestor_process"
			}
			return modemManagerOwnership{Owned: true, Reason: reason, Cgroup: holderCgroup}
		}

		parentPID, ok := readProcessParentPIDAt(procRoot, currentPID)
		if !ok || parentPID <= 1 || parentPID == currentPID {
			break
		}
		currentPID = parentPID
	}
	return modemManagerOwnership{Cgroup: holderCgroup}
}

func readProcessCgroupAt(procRoot string, pid int) string {
	data, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "cgroup"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func modemManagerServiceCgroup(value string) bool {
	for _, line := range strings.Split(value, "\n") {
		path := line
		if parts := strings.SplitN(line, ":", 3); len(parts) == 3 {
			path = parts[2]
		}
		for _, component := range strings.Split(path, "/") {
			if strings.EqualFold(strings.TrimSpace(component), "ModemManager.service") {
				return true
			}
		}
	}
	return false
}

func readProcessNameAt(procRoot string, pid int) string {
	base := filepath.Join(procRoot, strconv.Itoa(pid))
	if data, err := os.ReadFile(filepath.Join(base, "comm")); err == nil {
		if name := strings.TrimSpace(string(data)); name != "" {
			return name
		}
	}
	if target, err := os.Readlink(filepath.Join(base, "exe")); err == nil {
		return strings.TrimSuffix(filepath.Base(target), " (deleted)")
	}
	return ""
}

func readProcessParentPIDAt(procRoot string, pid int) (int, bool) {
	data, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "status"))
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || !strings.EqualFold(strings.TrimSuffix(fields[0], ":"), "PPid") {
			continue
		}
		parentPID, err := strconv.Atoi(fields[1])
		if err != nil {
			return 0, false
		}
		return parentPID, true
	}
	return 0, false
}
