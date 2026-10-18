package lens

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

func parseCmdToJSON(cmd string, data []byte, l *losses) ([]byte, error) {
	switch cmd {
	case "cmd:ps":
		return parsePS(data, l)
	case "cmd:df":
		return parseDF(data, l)
	case "cmd:ls-l":
		return parseLsL(data, l)
	case "cmd:du":
		return parseDU(data, l)
	case "cmd:wc":
		return parseWC(data, l)
	case "cmd:stat":
		return parseStat(data, l)
	case "cmd:id":
		return parseID(data, l)
	case "cmd:uname":
		return parseUname(data, l)
	case "cmd:tar-tv":
		return parseTarTV(data, l)
	case "cmd:ip-route":
		return parseIPRoute(data, l)
	case "cmd:netstat-rn":
		return parseNetstatRN(data, l)
	default:
		return nil, errorf(ExitUsage, "unknown cmd parser: %s", cmd)
	}
}

// ------------------------------------------------------------- cmd:ps

func parsePS(data []byte, l *losses) ([]byte, error) {
	text := strings.TrimSpace(string(data))
	if text == "" {
		return []byte("[]\n"), nil
	}
	lines := strings.Split(text, "\n")
	if len(lines) == 0 {
		return []byte("[]\n"), nil
	}

	headerLine := strings.TrimSpace(lines[0])
	headerTokens := strings.Fields(headerLine)
	if len(headerTokens) == 0 {
		return []byte("[]\n"), nil
	}

	pidIdx := -1
	for i, h := range headerTokens {
		if strings.EqualFold(h, "pid") {
			pidIdx = i
			break
		}
	}
	if pidIdx == -1 {
		return nil, errorf(ExitInvalid, "malformed ps output: missing PID column header")
	}

	var results []map[string]any
	colCount := len(headerTokens)

	for lineIdx := 1; lineIdx < len(lines); lineIdx++ {
		line := strings.TrimSpace(lines[lineIdx])
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < colCount {
			return nil, errorf(ExitInvalid, "malformed ps output on line %d: insufficient columns (%d < %d)", lineIdx+1, len(fields), colCount)
		}

		// Re-split so the final column (typically command/cmd) preserves spaces.
		rowTokens := make([]string, colCount)
		cur := strings.TrimLeft(line, " \t")
		for i := 0; i < colCount-1; i++ {
			spaceIdx := strings.IndexAny(cur, " \t")
			if spaceIdx == -1 {
				return nil, errorf(ExitInvalid, "malformed ps output on line %d: insufficient whitespace separation", lineIdx+1)
			}
			rowTokens[i] = cur[:spaceIdx]
			cur = strings.TrimLeft(cur[spaceIdx:], " \t")
		}
		rowTokens[colCount-1] = cur

		record := make(map[string]any)
		for i, h := range headerTokens {
			val := rowTokens[i]
			hLower := strings.ToLower(h)
			switch hLower {
			case "pid":
				n, err := strconv.ParseInt(val, 10, 64)
				if err != nil {
					return nil, errorf(ExitInvalid, "malformed ps output on line %d: invalid PID %q", lineIdx+1, val)
				}
				record["pid"] = n
			case "ppid":
				n, err := strconv.ParseInt(val, 10, 64)
				if err != nil {
					return nil, errorf(ExitInvalid, "malformed ps output on line %d: invalid PPID %q", lineIdx+1, val)
				}
				record["ppid"] = n
			case "c":
				n, err := strconv.ParseInt(val, 10, 64)
				if err == nil {
					record["c"] = n
				} else {
					record["c"] = val
				}
			case "%cpu", "cpu":
				f, err := strconv.ParseFloat(val, 64)
				if err == nil {
					record["cpu_percent"] = f
				} else {
					record["cpu_percent"] = val
				}
			case "%mem", "mem":
				f, err := strconv.ParseFloat(val, 64)
				if err == nil {
					record["mem_percent"] = f
				} else {
					record["mem_percent"] = val
				}
			case "vsz":
				n, err := strconv.ParseInt(val, 10, 64)
				if err == nil {
					record["vsz"] = n
				} else {
					record["vsz"] = val
				}
			case "rss":
				n, err := strconv.ParseInt(val, 10, 64)
				if err == nil {
					record["rss"] = n
				} else {
					record["rss"] = val
				}
			case "tt", "tty":
				if val == "?" || val == "-" {
					record["tty"] = nil
				} else {
					record["tty"] = val
				}
			case "stat", "state", "s":
				record["stat"] = val
			case "start", "started":
				record["start"] = val
			case "stime":
				record["stime"] = val
			case "time":
				record["time"] = val
			case "user":
				record["user"] = val
			case "uid":
				record["uid"] = val
			case "command":
				record["command"] = val
			case "cmd":
				record["cmd"] = val
			default:
				record[hLower] = val
			}
		}
		results = append(results, record)
	}

	if results == nil {
		results = []map[string]any{}
	}
	return marshalIndent(results)
}

// ------------------------------------------------------------- cmd:df

func parseDF(data []byte, l *losses) ([]byte, error) {
	text := strings.TrimSpace(string(data))
	if text == "" {
		return []byte("[]\n"), nil
	}
	lines := strings.Split(text, "\n")
	if len(lines) == 0 {
		return []byte("[]\n"), nil
	}

	headerLine := strings.TrimSpace(lines[0])
	if !strings.Contains(strings.ToLower(headerLine), "filesystem") {
		return nil, errorf(ExitInvalid, "malformed df output: missing Filesystem header")
	}

	headerTokens := strings.Fields(headerLine)
	var cols []string
	for i := 0; i < len(headerTokens); i++ {
		t := headerTokens[i]
		if strings.EqualFold(t, "mounted") && i+1 < len(headerTokens) && strings.EqualFold(headerTokens[i+1], "on") {
			cols = append(cols, "mounted_on")
			i++
			continue
		}
		cols = append(cols, normalizeDFCol(t))
	}

	hasMount := false
	for _, c := range cols {
		if c == "mounted_on" {
			hasMount = true
			break
		}
	}
	if !hasMount {
		return nil, errorf(ExitInvalid, "malformed df output: missing Mounted on header")
	}

	var results []map[string]any
	colCount := len(cols)

	for lineIdx := 1; lineIdx < len(lines); lineIdx++ {
		line := strings.TrimSpace(lines[lineIdx])
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		// Handle split filesystem line if filesystem name was long
		if len(fields) == 1 && lineIdx+1 < len(lines) {
			nextLine := strings.TrimSpace(lines[lineIdx+1])
			line = line + " " + nextLine
			fields = strings.Fields(line)
			lineIdx++
		}
		if len(fields) < colCount {
			return nil, errorf(ExitInvalid, "malformed df output on line %d: insufficient columns (%d < %d)", lineIdx+1, len(fields), colCount)
		}

		rowTokens := make([]string, colCount)
		cur := strings.TrimLeft(line, " \t")
		for i := 0; i < colCount-1; i++ {
			spaceIdx := strings.IndexAny(cur, " \t")
			if spaceIdx == -1 {
				return nil, errorf(ExitInvalid, "malformed df output on line %d: insufficient whitespace separation", lineIdx+1)
			}
			rowTokens[i] = cur[:spaceIdx]
			cur = strings.TrimLeft(cur[spaceIdx:], " \t")
		}
		rowTokens[colCount-1] = cur

		record := make(map[string]any)
		for i, c := range cols {
			val := rowTokens[i]
			switch c {
			case "use_percent", "iused_percent":
				valClean := strings.TrimSuffix(val, "%")
				n, err := strconv.Atoi(valClean)
				if err != nil {
					return nil, errorf(ExitInvalid, "malformed df output on line %d: invalid percentage %q", lineIdx+1, val)
				}
				record[c] = n
			case "1k_blocks", "512_blocks", "blocks", "used", "available", "iused", "ifree":
				n, err := strconv.ParseInt(val, 10, 64)
				if err == nil {
					record[c] = n
				} else {
					record[c] = val
				}
			default:
				record[c] = val
			}
		}
		results = append(results, record)
	}

	if results == nil {
		results = []map[string]any{}
	}
	return marshalIndent(results)
}

func normalizeDFCol(h string) string {
	switch strings.ToLower(h) {
	case "filesystem":
		return "filesystem"
	case "1k-blocks", "1024-blocks":
		return "1k_blocks"
	case "512-blocks":
		return "512_blocks"
	case "size", "blocks":
		return "blocks"
	case "used":
		return "used"
	case "available", "avail":
		return "available"
	case "use%", "capacity":
		return "use_percent"
	case "mounted":
		return "mounted_on"
	case "iused":
		return "iused"
	case "ifree":
		return "ifree"
	case "%iused", "iuse%":
		return "iused_percent"
	default:
		return strings.ToLower(strings.ReplaceAll(h, "-", "_"))
	}
}

// ------------------------------------------------------------- cmd:ls-l

var lsPermRe = regexp.MustCompile(`^[-dclbpsDrw][-r][-w][-xstST][-r][-w][-xstST][-r][-w][-xstST][@+\.]?$`)

func parseLsL(data []byte, l *losses) ([]byte, error) {
	text := strings.TrimSpace(string(data))
	if text == "" {
		return []byte("[]\n"), nil
	}
	lines := strings.Split(text, "\n")
	var results []map[string]any

	for lineIdx, rawLine := range lines {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "total ") || strings.HasPrefix(line, "total:") {
			continue
		}
		if strings.HasSuffix(line, ":") && !strings.Contains(line, " ") {
			continue
		}

		fields := strings.Fields(line)
		if len(fields) < 8 {
			return nil, errorf(ExitInvalid, "malformed ls -l output on line %d: insufficient fields (%d)", lineIdx+1, len(fields))
		}

		perms := fields[0]
		if !lsPermRe.MatchString(perms) {
			return nil, errorf(ExitInvalid, "malformed ls -l output on line %d: invalid permissions %q", lineIdx+1, perms)
		}

		links, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return nil, errorf(ExitInvalid, "malformed ls -l output on line %d: invalid links %q", lineIdx+1, fields[1])
		}

		owner := fields[2]
		group := fields[3]

		var size int64
		var dateTokens []string
		nameStartIdx := 0

		if strings.HasSuffix(fields[4], ",") && len(fields) >= 9 {
			// Device node: major, minor
			majStr := strings.TrimSuffix(fields[4], ",")
			_, err1 := strconv.ParseInt(majStr, 10, 64)
			_, err2 := strconv.ParseInt(fields[5], 10, 64)
			if err1 != nil || err2 != nil {
				return nil, errorf(ExitInvalid, "malformed ls -l output on line %d: invalid device numbers %q %q", lineIdx+1, fields[4], fields[5])
			}
			dateTokens = fields[6:9]
			nameStartIdx = 9
		} else {
			s, err := strconv.ParseInt(fields[4], 10, 64)
			if err != nil {
				return nil, errorf(ExitInvalid, "malformed ls -l output on line %d: invalid size %q", lineIdx+1, fields[4])
			}
			size = s
			if len(fields[5]) == 10 && fields[5][4] == '-' && fields[5][7] == '-' {
				// ISO date: YYYY-MM-DD HH:MM
				if len(fields) < 8 {
					return nil, errorf(ExitInvalid, "malformed ls -l output on line %d: truncated date/name", lineIdx+1)
				}
				dateTokens = fields[5:7]
				nameStartIdx = 7
			} else {
				// Standard BSD/GNU: Month Day Time/Year
				if len(fields) < 9 {
					return nil, errorf(ExitInvalid, "malformed ls -l output on line %d: truncated date/name", lineIdx+1)
				}
				dateTokens = fields[5:8]
				nameStartIdx = 8
			}
		}

		if nameStartIdx >= len(fields) {
			return nil, errorf(ExitInvalid, "malformed ls -l output on line %d: missing filename", lineIdx+1)
		}

		nameRaw := strings.Join(fields[nameStartIdx:], " ")
		filename := nameRaw
		linkTo := ""
		if idx := strings.Index(nameRaw, " -> "); idx != -1 {
			filename = nameRaw[:idx]
			linkTo = nameRaw[idx+4:]
		}

		record := map[string]any{
			"filename": filename,
			"flags":    perms,
			"links":    links,
			"owner":    owner,
			"group":    group,
			"size":     size,
			"date":     strings.Join(dateTokens, " "),
		}
		if linkTo != "" {
			record["link_to"] = linkTo
		}
		results = append(results, record)
	}

	if results == nil {
		results = []map[string]any{}
	}
	return marshalIndent(results)
}

// ------------------------------------------------------------- cmd:du

func parseDU(data []byte, l *losses) ([]byte, error) {
	text := strings.TrimSpace(string(data))
	if text == "" {
		return []byte("[]\n"), nil
	}
	lines := strings.Split(text, "\n")
	var results []map[string]any

	for lineIdx, rawLine := range lines {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		spaceIdx := strings.IndexAny(line, " \t")
		if spaceIdx == -1 {
			return nil, errorf(ExitInvalid, "malformed du output on line %d: missing path separation: %q", lineIdx+1, line)
		}
		blockStr := line[:spaceIdx]
		pathStr := strings.TrimLeft(line[spaceIdx:], " \t")
		blocks, err := strconv.ParseInt(blockStr, 10, 64)
		if err != nil {
			return nil, errorf(ExitInvalid, "malformed du output on line %d: invalid blocks %q", lineIdx+1, blockStr)
		}
		results = append(results, map[string]any{
			"blocks": blocks,
			"path":   pathStr,
		})
	}

	if results == nil {
		results = []map[string]any{}
	}
	return marshalIndent(results)
}

// ------------------------------------------------------------- cmd:wc

func parseWC(data []byte, l *losses) ([]byte, error) {
	text := strings.TrimSpace(string(data))
	if text == "" {
		return []byte("[]\n"), nil
	}
	lines := strings.Split(text, "\n")
	var results []map[string]any

	for lineIdx, rawLine := range lines {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		tokens := strings.Fields(line)
		if len(tokens) == 0 {
			continue
		}

		if len(tokens) >= 3 {
			lCount, errL := strconv.ParseInt(tokens[0], 10, 64)
			wCount, errW := strconv.ParseInt(tokens[1], 10, 64)
			bCount, errB := strconv.ParseInt(tokens[2], 10, 64)
			if errL == nil && errW == nil && errB == nil {
				record := map[string]any{
					"lines": lCount,
					"words": wCount,
					"bytes": bCount,
				}
				if len(tokens) > 3 {
					record["file"] = strings.Join(tokens[3:], " ")
				}
				results = append(results, record)
				continue
			}
		}

		if len(tokens) == 2 {
			count, err := strconv.ParseInt(tokens[0], 10, 64)
			if err == nil {
				results = append(results, map[string]any{
					"lines": count,
					"file":  tokens[1],
				})
				continue
			}
		}

		if len(tokens) == 1 {
			count, err := strconv.ParseInt(tokens[0], 10, 64)
			if err == nil {
				results = append(results, map[string]any{
					"lines": count,
				})
				continue
			}
		}

		return nil, errorf(ExitInvalid, "malformed wc output on line %d: %q", lineIdx+1, line)
	}

	if results == nil {
		results = []map[string]any{}
	}
	return marshalIndent(results)
}

// ------------------------------------------------------------- cmd:stat

var (
	statFileRe   = regexp.MustCompile(`(?m)^\s*File:\s*(.+)$`)
	statSizeRe   = regexp.MustCompile(`(?m)^\s*Size:\s*(\d+)\s+Blocks:\s*(\d+)\s+IO Block:\s*(\d+)\s+(.+)$`)
	statDevRe    = regexp.MustCompile(`(?m)^\s*Device:\s*([^\s\t]+)\s+Inode:\s*(\d+)\s+Links:\s*(\d+)`)
	statAccessRe = regexp.MustCompile(`(?m)^\s*Access:\s*\(([0-9]+)/([^\)]+)\)\s+Uid:\s*\(\s*(\d+)/\s*([^\)]*)\)\s+Gid:\s*\(\s*(\d+)/\s*([^\)]*)\)`)
	statTimeRe   = regexp.MustCompile(`(?m)^\s*(Access|Modify|Change|Birth):\s*(.+)$`)
)

func parseStat(data []byte, l *losses) ([]byte, error) {
	text := strings.TrimSpace(string(data))
	if text == "" {
		return nil, errorf(ExitInvalid, "malformed stat output: empty input")
	}
	if !strings.Contains(text, "File:") {
		return nil, errorf(ExitInvalid, "malformed stat output: missing File: header")
	}

	// Split by File: boundary
	rawBlocks := strings.Split("\n"+text, "\n  File:")
	if len(rawBlocks) <= 1 {
		rawBlocks = strings.Split("\n"+text, "\nFile:")
	}

	var results []map[string]any

	for blockIdx, rawBlock := range rawBlocks {
		if strings.TrimSpace(rawBlock) == "" {
			continue
		}
		block := "File:" + rawBlock

		fileMatch := statFileRe.FindStringSubmatch(block)
		if len(fileMatch) < 2 {
			return nil, errorf(ExitInvalid, "malformed stat output block %d: missing File name", blockIdx)
		}
		rawFileName := strings.Trim(strings.TrimSpace(fileMatch[1]), `"`)
		fileName := rawFileName
		linkTo := ""
		if idx := strings.Index(rawFileName, " -> "); idx != -1 {
			fileName = rawFileName[:idx]
			linkTo = rawFileName[idx+4:]
		}

		sizeMatch := statSizeRe.FindStringSubmatch(block)
		if len(sizeMatch) < 5 {
			return nil, errorf(ExitInvalid, "malformed stat output block %d: missing Size line", blockIdx)
		}
		size, _ := strconv.ParseInt(sizeMatch[1], 10, 64)
		blocks, _ := strconv.ParseInt(sizeMatch[2], 10, 64)
		ioBlock, _ := strconv.ParseInt(sizeMatch[3], 10, 64)
		fileType := strings.TrimSpace(sizeMatch[4])

		devMatch := statDevRe.FindStringSubmatch(block)
		if len(devMatch) < 4 {
			return nil, errorf(ExitInvalid, "malformed stat output block %d: missing Device line", blockIdx)
		}
		device := devMatch[1]
		inode, _ := strconv.ParseInt(devMatch[2], 10, 64)
		links, _ := strconv.ParseInt(devMatch[3], 10, 64)

		accMatch := statAccessRe.FindStringSubmatch(block)
		if len(accMatch) < 7 {
			return nil, errorf(ExitInvalid, "malformed stat output block %d: missing Access permission line", blockIdx)
		}
		access := accMatch[1]
		flags := accMatch[2]
		uid, _ := strconv.ParseInt(accMatch[3], 10, 64)
		user := strings.TrimSpace(accMatch[4])
		gid, _ := strconv.ParseInt(accMatch[5], 10, 64)
		group := strings.TrimSpace(accMatch[6])

		record := map[string]any{
			"file":      fileName,
			"size":      size,
			"blocks":    blocks,
			"io_blocks": ioBlock,
			"type":      fileType,
			"device":    device,
			"inode":     inode,
			"links":     links,
			"access":    access,
			"flags":     flags,
			"uid":       uid,
			"user":      user,
			"gid":       gid,
			"group":     group,
		}
		if linkTo != "" {
			record["link_to"] = linkTo
		}

		timeMatches := statTimeRe.FindAllStringSubmatch(block, -1)
		for _, tm := range timeMatches {
			kind := tm[1]
			val := strings.TrimSpace(tm[2])
			switch kind {
			case "Access":
				record["access_time"] = val
			case "Modify":
				record["modify_time"] = val
			case "Change":
				record["change_time"] = val
			case "Birth":
				if val != "-" && val != "" {
					record["birth_time"] = val
				}
			}
		}
		results = append(results, record)
	}

	if len(results) == 0 {
		return nil, errorf(ExitInvalid, "malformed stat output: no stat entries parsed")
	}
	return marshalIndent(results)
}

// ------------------------------------------------------------- cmd:id

func parseID(data []byte, l *losses) ([]byte, error) {
	text := strings.TrimSpace(string(data))
	if text == "" {
		return nil, errorf(ExitInvalid, "malformed id output: empty input")
	}
	if !strings.Contains(text, "uid=") || !strings.Contains(text, "gid=") {
		return nil, errorf(ExitInvalid, "malformed id output: missing uid/gid")
	}

	tokens := strings.Fields(text)
	result := make(map[string]any)

	for _, tok := range tokens {
		parts := strings.SplitN(tok, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key, val := parts[0], parts[1]
		switch key {
		case "uid", "gid", "euid", "egid":
			id, name, err := parseIDToken(val)
			if err != nil {
				return nil, errorf(ExitInvalid, "malformed id output token %q: %v", tok, err)
			}
			result[key] = map[string]any{"id": id, "name": name}
		case "groups":
			groupTokens := strings.Split(val, ",")
			var groups []map[string]any
			for _, gt := range groupTokens {
				gt = strings.TrimSpace(gt)
				if gt == "" {
					continue
				}
				gid, gname, err := parseIDToken(gt)
				if err != nil {
					return nil, errorf(ExitInvalid, "malformed id groups token %q: %v", gt, err)
				}
				groups = append(groups, map[string]any{"id": gid, "name": gname})
			}
			if groups == nil {
				groups = []map[string]any{}
			}
			result["groups"] = groups
		case "context":
			result["context"] = val
		}
	}

	if _, ok := result["uid"]; !ok {
		return nil, errorf(ExitInvalid, "malformed id output: missing uid")
	}
	if _, ok := result["gid"]; !ok {
		return nil, errorf(ExitInvalid, "malformed id output: missing gid")
	}

	return marshalIndent(result)
}

func parseIDToken(s string) (int64, string, error) {
	// e.g. 1000(alice) or 1000
	if idx := strings.Index(s, "("); idx != -1 && strings.HasSuffix(s, ")") {
		numStr := s[:idx]
		name := s[idx+1 : len(s)-1]
		id, err := strconv.ParseInt(numStr, 10, 64)
		if err != nil {
			return 0, "", fmt.Errorf("invalid numeric id %q", numStr)
		}
		return id, name, nil
	}
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, "", fmt.Errorf("invalid numeric id %q", s)
	}
	return id, "", nil
}

// ------------------------------------------------------------- cmd:uname

func parseUname(data []byte, l *losses) ([]byte, error) {
	text := strings.TrimSpace(string(data))
	if text == "" {
		return nil, errorf(ExitInvalid, "malformed uname output: empty input")
	}
	words := strings.Fields(text)
	if len(words) == 0 {
		return nil, errorf(ExitInvalid, "malformed uname output: empty input")
	}

	result := make(map[string]any)
	if len(words) == 1 {
		result["kernel_name"] = words[0]
		return marshalIndent(result)
	}

	if len(words) < 3 {
		result["kernel_name"] = words[0]
		result["node_name"] = words[1]
		return marshalIndent(result)
	}

	result["kernel_name"] = words[0]
	result["node_name"] = words[1]
	result["kernel_release"] = words[2]

	last := words[len(words)-1]
	if len(words) >= 7 && (last == "GNU/Linux" || last == "Linux") {
		result["operating_system"] = last
		result["hardware_platform"] = words[len(words)-2]
		result["processor"] = words[len(words)-3]
		result["machine"] = words[len(words)-4]
		result["kernel_version"] = strings.Join(words[3:len(words)-4], " ")
	} else if len(words) >= 4 {
		result["machine"] = last
		result["kernel_version"] = strings.Join(words[3:len(words)-1], " ")
	}

	return marshalIndent(result)
}

// ------------------------------------------------------------- cmd:tar-tv

func parseTarTV(data []byte, l *losses) ([]byte, error) {
	text := strings.TrimSpace(string(data))
	if text == "" {
		return []byte("[]\n"), nil
	}
	lines := strings.Split(text, "\n")
	var results []map[string]any

	for lineIdx, rawLine := range lines {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 6 {
			return nil, errorf(ExitInvalid, "malformed tar -tv output on line %d: insufficient columns (%d < 6)", lineIdx+1, len(fields))
		}

		mode := fields[0]
		if len(mode) < 9 || !strings.ContainsAny(string(mode[0]), "-dclhbp") {
			return nil, errorf(ExitInvalid, "malformed tar -tv output on line %d: invalid permissions %q", lineIdx+1, mode)
		}

		userGroup := fields[1]
		owner, group := userGroup, ""
		if idx := strings.Index(userGroup, "/"); idx != -1 {
			owner = userGroup[:idx]
			group = userGroup[idx+1:]
		}

		var size int64
		if !strings.Contains(fields[2], ",") {
			s, err := strconv.ParseInt(fields[2], 10, 64)
			if err != nil {
				return nil, errorf(ExitInvalid, "malformed tar -tv output on line %d: invalid size %q", lineIdx+1, fields[2])
			}
			size = s
		}

		date := fields[3] + " " + fields[4]
		nameStartIdx := 5
		if len(fields[3]) != 10 && len(fields) >= 7 {
			// Month Day Time/Year format: e.g. Oct 17 12:00
			date = fields[3] + " " + fields[4] + " " + fields[5]
			nameStartIdx = 6
		}

		if nameStartIdx >= len(fields) {
			return nil, errorf(ExitInvalid, "malformed tar -tv output on line %d: missing name", lineIdx+1)
		}

		nameRaw := strings.Join(fields[nameStartIdx:], " ")
		filename := nameRaw
		linkTo := ""
		if idx := strings.Index(nameRaw, " -> "); idx != -1 {
			filename = nameRaw[:idx]
			linkTo = nameRaw[idx+4:]
		} else if idx := strings.Index(nameRaw, " link to "); idx != -1 {
			filename = nameRaw[:idx]
			linkTo = nameRaw[idx+9:]
		}

		record := map[string]any{
			"filename": filename,
			"flags":    mode,
			"owner":    owner,
			"group":    group,
			"size":     size,
			"date":     date,
		}
		if linkTo != "" {
			record["link_to"] = linkTo
		}
		results = append(results, record)
	}

	if results == nil {
		results = []map[string]any{}
	}
	return marshalIndent(results)
}

// ------------------------------------------------------------- cmd:ip-route

func parseIPRoute(data []byte, l *losses) ([]byte, error) {
	text := strings.TrimSpace(string(data))
	if text == "" {
		return []byte("[]\n"), nil
	}
	lines := strings.Split(text, "\n")
	routeTypes := map[string]bool{
		"unicast": true, "local": true, "broadcast": true, "multicast": true,
		"blackhole": true, "unreachable": true, "prohibit": true, "throw": true, "nat": true,
	}
	knownKV := map[string]bool{
		"via": true, "dev": true, "metric": true, "proto": true, "scope": true,
		"src": true, "table": true, "mtu": true, "advmss": true, "hoplimit": true,
		"pref": true, "expires": true, "prefsrc": true, "onlink": false,
	}

	var results []map[string]any
	for lineIdx, rawLine := range lines {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}

		record := make(map[string]any)
		pos := 0
		// Detect leading type prefix.
		if routeTypes[fields[0]] {
			if len(fields) < 2 {
				return nil, errorf(ExitInvalid, "malformed ip route output on line %d: missing destination after type %q", lineIdx+1, fields[0])
			}
			record["type"] = fields[0]
			record["destination"] = fields[1]
			pos = 2
		} else {
			record["destination"] = fields[0]
			pos = 1
		}

		dst, _ := record["destination"].(string)
		if dst == "" {
			return nil, errorf(ExitInvalid, "malformed ip route output on line %d: missing destination", lineIdx+1)
		}

		var extras []string
		var flags []string
		i := pos
		for i < len(fields) {
			tok := fields[i]
			switch tok {
			case "via":
				if i+1 >= len(fields) {
					return nil, errorf(ExitInvalid, "malformed ip route output on line %d: via without gateway", lineIdx+1)
				}
				if _, exists := record["gateway"]; exists {
					return nil, errorf(ExitInvalid, "malformed ip route output on line %d: duplicate gateway", lineIdx+1)
				}
				gw := fields[i+1]
				if gw == "" {
					return nil, errorf(ExitInvalid, "malformed ip route output on line %d: empty gateway", lineIdx+1)
				}
				record["gateway"] = gw
				i += 2
			case "dev":
				if i+1 >= len(fields) {
					return nil, errorf(ExitInvalid, "malformed ip route output on line %d: dev without interface", lineIdx+1)
				}
				record["interface"] = fields[i+1]
				i += 2
			case "metric":
				if i+1 >= len(fields) {
					return nil, errorf(ExitInvalid, "malformed ip route output on line %d: metric without value", lineIdx+1)
				}
				val := fields[i+1]
				if n, err := strconv.ParseInt(val, 10, 64); err == nil {
					record["metric"] = n
				} else {
					record["metric"] = val
				}
				i += 2
			case "proto", "scope", "src", "table":
				if i+1 >= len(fields) {
					return nil, errorf(ExitInvalid, "malformed ip route output on line %d: %s without value", lineIdx+1, tok)
				}
				record[tok] = fields[i+1]
				i += 2
			case "mtu", "advmss", "hoplimit":
				if i+1 >= len(fields) {
					return nil, errorf(ExitInvalid, "malformed ip route output on line %d: %s without value", lineIdx+1, tok)
				}
				val := fields[i+1]
				if n, err := strconv.ParseInt(val, 10, 64); err == nil {
					record[tok] = n
				} else {
					record[tok] = val
				}
				i += 2
			case "onlink", "linkdown":
				flags = append(flags, tok)
				i++
			default:
				// Check if token is a known keyword requiring value but we missed.
				if knownKV[tok] {
					return nil, errorf(ExitInvalid, "malformed ip route output on line %d: %s without value", lineIdx+1, tok)
				}
				// Unknown key-value: if next token exists and is not a known keyword/flag, treat as kv.
				if i+1 < len(fields) && !knownKV[fields[i+1]] && fields[i+1] != "via" && fields[i+1] != "dev" && fields[i+1] != "metric" && fields[i+1] != "onlink" && fields[i+1] != "linkdown" && !routeTypes[fields[i+1]] {
					// Heuristic: if tok looks like a key and next looks like value, preserve as extra kv.
					// But to preserve unknown fields deterministically, store as extra string.
					// For known pattern key value, keep as field.
					// We preserve unknown fields by storing under their literal key if not already present.
					key := strings.ToLower(tok)
					// Avoid colliding with known keys.
					if _, exists := record[key]; !exists {
						record[key] = fields[i+1]
					} else {
						extras = append(extras, tok, fields[i+1])
					}
					i += 2
				} else {
					extras = append(extras, tok)
					i++
				}
			}
		}
		if len(flags) > 0 {
			if len(flags) == 1 {
				record["flags"] = flags[0]
			} else {
				record["flags"] = strings.Join(flags, " ")
			}
		}
		if len(extras) > 0 {
			record["extra"] = strings.Join(extras, " ")
		}
		// Preserve raw line for full fidelity.
		record["raw"] = line
		results = append(results, record)
	}
	if results == nil {
		results = []map[string]any{}
	}
	return marshalIndent(results)
}

// ------------------------------------------------------------- cmd:netstat-rn

func parseNetstatRN(data []byte, l *losses) ([]byte, error) {
	text := strings.TrimSpace(string(data))
	if text == "" {
		return []byte("[]\n"), nil
	}
	lines := strings.Split(text, "\n")

	// Find header line containing Destination and Gateway and Flags.
	headerIdx := -1
	var headerFields []string
	for idx, rawLine := range lines {
		trimmed := strings.TrimSpace(rawLine)
		if trimmed == "" {
			continue
		}
		lower := strings.ToLower(trimmed)
		if strings.Contains(lower, "destination") && strings.Contains(lower, "gateway") && strings.Contains(lower, "flags") {
			headerIdx = idx
			headerFields = strings.Fields(trimmed)
			break
		}
	}
	if headerIdx == -1 {
		return nil, errorf(ExitInvalid, "malformed netstat -rn output: missing header (Destination Gateway Flags)")
	}

	// Normalize header column names to lowercase.
	normHeaders := make([]string, len(headerFields))
	for i, h := range headerFields {
		normHeaders[i] = strings.ToLower(h)
	}

	// Determine column indices for known logical fields.
	destIdx, gwIdx, flagsIdx, ifaceIdx := -1, -1, -1, -1
	expireIdx := -1
	for i, h := range normHeaders {
		switch h {
		case "destination":
			destIdx = i
		case "gateway":
			gwIdx = i
		case "flags":
			flagsIdx = i
		case "netif", "iface", "interface", "if":
			ifaceIdx = i
		case "expire":
			expireIdx = i
		}
	}
	if destIdx == -1 || gwIdx == -1 || flagsIdx == -1 || ifaceIdx == -1 {
		return nil, errorf(ExitInvalid, "malformed netstat -rn output: missing required columns")
	}

	var results []map[string]any
	for lineIdx := headerIdx + 1; lineIdx < len(lines); lineIdx++ {
		rawLine := lines[lineIdx]
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		// Skip section headers like "Routing tables", "Internet:", "Internet6:"
		if strings.HasSuffix(line, ":") && !strings.Contains(line, " ") {
			continue
		}
		lowerLine := strings.ToLower(line)
		if lowerLine == "routing tables" || lowerLine == "internet:" || lowerLine == "internet6:" {
			continue
		}
		// Skip lines that look like a repeated header.
		if strings.Contains(lowerLine, "destination") && strings.Contains(lowerLine, "gateway") {
			continue
		}

		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) < len(normHeaders) {
			// For BSD/macOS, Expire may be optional; allow one missing trailing column.
			// But fail closed if we lack required 4.
			if len(fields) < 4 {
				return nil, errorf(ExitInvalid, "malformed netstat -rn output on line %d: insufficient columns (%d < 4)", lineIdx+1, len(fields))
			}
			// If header has 5 cols and row has 4, map first 4; leave expire empty.
		}
		if len(fields) < 4 {
			return nil, errorf(ExitInvalid, "malformed netstat -rn output on line %d: insufficient columns (%d < 4)", lineIdx+1, len(fields))
		}

		// Map by position; header order is expected to be Destination Gateway Flags Netif [Expire ...]
		// Use indices to support alternative orderings.
		get := func(idx int) string {
			if idx >= 0 && idx < len(fields) {
				return fields[idx]
			}
			return ""
		}
		// If headers and fields align, use direct indices; otherwise fallback to positional 0..3.
		var dest, gateway, flags, iface, expire string
		if destIdx < len(fields) && gwIdx < len(fields) && flagsIdx < len(fields) && ifaceIdx < len(fields) {
			dest = get(destIdx)
			gateway = get(gwIdx)
			flags = get(flagsIdx)
			iface = get(ifaceIdx)
			if expireIdx >= 0 {
				expire = get(expireIdx)
			}
		} else {
			dest = fields[0]
			gateway = fields[1]
			flags = fields[2]
			iface = fields[3]
			if len(fields) > 4 {
				expire = fields[4]
			}
		}
		if dest == "" || gateway == "" || flags == "" || iface == "" {
			return nil, errorf(ExitInvalid, "malformed netstat -rn output on line %d: missing required field", lineIdx+1)
		}

		record := map[string]any{
			"destination": dest,
			"gateway":     gateway,
			"flags":       flags,
			"interface":   iface,
		}
		if expire != "" && expire != "-" {
			record["expire"] = expire
		}
		// Preserve metric-like info if flags contain numeric? Flags already captured.
		// Preserve unknown extra columns beyond header.
		if len(fields) > len(normHeaders) {
			extra := strings.Join(fields[len(normHeaders):], " ")
			if extra != "" {
				record["extra"] = extra
			}
		} else if len(fields) > 5 && len(normHeaders) <= 5 {
			// Already handled via Expire minimal check; nothing
		}
		// Also preserve any additional raw tokens if header had refs/use/mtu etc. Store generically.
		for i, h := range normHeaders {
			if h == "destination" || h == "gateway" || h == "flags" || h == "netif" || h == "iface" || h == "interface" || h == "if" || h == "expire" {
				continue
			}
			if i < len(fields) {
				val := fields[i]
				if val != "" && val != "-" {
					if n, err := strconv.ParseInt(val, 10, 64); err == nil {
						record[h] = n
					} else {
						record[h] = val
					}
				}
			}
		}
		record["raw"] = line
		results = append(results, record)
	}

	if results == nil {
		results = []map[string]any{}
	}
	return marshalIndent(results)
}

func marshalIndent(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
