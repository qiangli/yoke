package weave

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/capability"
	"github.com/qiangli/yoke/pkg/ladder"
	"github.com/spf13/cobra"
)

type plantedEntry struct {
	Case    string `json:"case"`
	Planted bool   `json:"planted"`
	Answer  string `json:"answer,omitempty"`
}

type panelDrawEvent struct {
	Case    string   `json:"case"`
	Size    int      `json:"size"`
	Members []string `json:"members"`
}

type panelVoteEvent struct {
	Case       string             `json:"case"`
	Agent      string             `json:"agent"`
	Verdict    string             `json:"verdict"`
	Confidence float64            `json:"confidence"`
	Rubric     map[string]float64 `json:"rubric,omitempty"`
}

type panelVerdictEvent struct {
	Case    string         `json:"case"`
	Verdict string         `json:"verdict,omitempty"`
	Tally   map[string]int `json:"tally"`
	ToOwner bool           `json:"to_owner,omitempty"`
	Reason  string         `json:"reason,omitempty"`
	Season  int            `json:"season,omitempty"`
}

func panelCaseSeed(sprintUUID, caseID string) int64 {
	h := fnv.New64a()
	h.Write([]byte(sprintUUID + caseID))
	return int64(h.Sum64())
}

func sprintPanelsDir(sprintID int64) (string, error) {
	home := os.Getenv("BASHY_HOME")
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		home = filepath.Join(userHome, ".bashy")
	}
	return filepath.Join(home, "sprint", "panels", strconv.FormatInt(sprintID, 10)), nil
}

func sprintPlantedPath(sprintID int64) (string, error) {
	dir, err := sprintPanelsDir(sprintID)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "planted.json"), nil
}

func readPlantedFile(path string) (map[string]plantedEntry, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return make(map[string]plantedEntry), nil
	}
	if err != nil {
		return nil, err
	}
	var m map[string]plantedEntry
	if err := json.Unmarshal(data, &m); err == nil {
		return m, nil
	}
	var strMap map[string]string
	if err := json.Unmarshal(data, &strMap); err == nil {
		res := make(map[string]plantedEntry)
		for k, v := range strMap {
			res[k] = plantedEntry{Case: k, Planted: true, Answer: v}
		}
		return res, nil
	}
	var list []plantedEntry
	if err := json.Unmarshal(data, &list); err == nil {
		res := make(map[string]plantedEntry)
		for _, e := range list {
			res[e.Case] = e
		}
		return res, nil
	}
	return make(map[string]plantedEntry), nil
}

func savePlantedFile(path string, m map[string]plantedEntry) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

func appendLadderEventDirectly(path string, e ladder.Event) error {
	if path == "" {
		path = ladder.DefaultStorePath()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	if e.Schema == "" {
		e.Schema = ladder.EventSchema
	}
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(data, '\n'))
	return err
}

func auditSample(caseID string, season int, rate float64) bool {
	h := fnv.New64a()
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], uint64(season))
	h.Write(b[:])
	h.Write([]byte(caseID))
	threshold := uint64(rate * 10000)
	return h.Sum64()%10000 < threshold
}

func buildPanelPool(rep ladder.ReplayResult, season int) ([]ladder.PanelMember, error) {
	cat := fleetCatalog()
	lines := capability.LadderLines(rep, season)
	agents, _ := cat.Agents()
	var pool []ladder.PanelMember
	seen := map[string]bool{}
	for _, a := range agents {
		if a.Ephemeral || a.ClonedFrom != "" {
			continue
		}
		binding, tool, model, err := cat.Binding(a.Name)
		if err != nil {
			continue
		}
		if seen[a.Name] {
			continue
		}
		seen[a.Name] = true

		key := binding.MatrixKey()
		peg := a.Band
		if peg == 0 && model.Band > 0 {
			peg = model.Band
		}

		profile := ladder.Profile{}
		if rec := rep.Agents[key]; rec != nil {
			profile.Standings = rec.Standings
			profile.Certs = rec.Certs
			profile.Provisional = rec.Provisional
			if n := len(rec.Certs); n > 0 {
				profile.ModelVersion = rec.Certs[n-1].ModelVersion
			}
		} else if rec := rep.Agents[a.Name]; rec != nil {
			profile.Standings = rec.Standings
			profile.Certs = rec.Certs
			profile.Provisional = rec.Provisional
			if n := len(rec.Certs); n > 0 {
				profile.ModelVersion = rec.Certs[n-1].ModelVersion
			}
		}

		isL5 := false
		if profile.Provisional >= 5 {
			isL5 = true
		} else {
			band, _ := ladder.DeriveBand(profile, lines, season)
			if band >= 5 {
				isL5 = true
			} else if band == 0 && profile.Standings[ladder.DutyJudge].Events == 0 && peg >= 5 {
				isL5 = true
			}
		}
		if !isL5 {
			continue
		}

		pool = append(pool, ladder.PanelMember{
			Agent:  a.Name,
			Vendor: tool.Name,
			Judge:  profile.Standings[ladder.DutyJudge],
			Manage: profile.Standings[ladder.DutyManage],
		})
	}
	return pool, nil
}

func newSprintPanelCmd() *cobra.Command {
	var (
		caseID        string
		kind          string
		points        int
		authorVendor  string
		lowConfidence bool
		agent         string
		verdict       string
		confidence    float64
		rubric        []string
		season        int
		rate          float64
		answer        string
	)

	cmd := &cobra.Command{
		Use:   "panel <sprint> <draw|vote|verdict|audit>",
		Short: "Manage judge panels for sprint decisions",
		Args:  cobra.MinimumNArgs(2),
	}

	cmd.Flags().StringVar(&caseID, "case", "", "panel case id")
	cmd.Flags().StringVar(&kind, "kind", "low-stakes", "panel case kind (low-stakes|design|merge|dispute|escalated)")
	cmd.Flags().IntVar(&points, "points", 0, "story points")
	cmd.Flags().StringVar(&authorVendor, "author-vendor", "", "author vendor to exclude")
	cmd.Flags().BoolVar(&lowConfidence, "low-confidence", false, "case marked low confidence")
	cmd.Flags().StringVar(&agent, "agent", "", "voter agent name")
	cmd.Flags().StringVar(&verdict, "verdict", "", "verdict (e.g. accept, reject)")
	cmd.Flags().Float64Var(&confidence, "confidence", 0.0, "verdict confidence (0..1)")
	cmd.Flags().StringArrayVar(&rubric, "rubric", nil, "rubric scores k=v")
	cmd.Flags().IntVar(&season, "season", 0, "season for audit")
	cmd.Flags().Float64Var(&rate, "rate", 0.10, "audit sample rate (default 0.1)")
	cmd.Flags().StringVar(&answer, "answer", "", "known planted calibration answer")

	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if len(args) < 2 {
			return fmt.Errorf("sprint panel requires <sprint> and <draw|vote|verdict|audit>")
		}
		sprintArgStr := args[0]
		subverb := args[1]
		if args[0] == "draw" || args[0] == "vote" || args[0] == "verdict" || args[0] == "audit" {
			subverb = args[0]
			sprintArgStr = args[1]
		}

		flags := &weaveOutputFlags{}
		id, err := sprintArg(cmd, flags.mode(), "sprint panel", sprintArgStr)
		if err != nil {
			return err
		}

		switch subverb {
		case "draw":
			return runSprintPanelDraw(cmd, id, flags, caseID, kind, points, authorVendor, lowConfidence, answer)
		case "vote":
			return runSprintPanelVote(cmd, id, flags, caseID, agent, verdict, confidence, rubric)
		case "verdict":
			return runSprintPanelVerdict(cmd, id, flags, caseID)
		case "audit":
			return runSprintPanelAudit(cmd, id, flags, season, rate)
		default:
			return fmt.Errorf("unknown panel subverb %q; want draw|vote|verdict|audit", subverb)
		}
	}
	return cmd
}

func runSprintPanelDraw(cmd *cobra.Command, id int64, flags *weaveOutputFlags, caseID, kind string, points int, authorVendor string, lowConfidence bool, answer string) error {
	caseID = strings.TrimSpace(caseID)
	if caseID == "" {
		return fmt.Errorf("--case is required")
	}
	kind = strings.TrimSpace(kind)
	if kind == "" {
		kind = "low-stakes"
	}
	switch kind {
	case "low-stakes", "design", "merge", "dispute", "escalated":
	default:
		return fmt.Errorf("invalid --kind %q; want low-stakes|design|merge|dispute|escalated", kind)
	}

	board, err := sprintStoreDir()
	if err != nil {
		return err
	}

	storePath := ladder.DefaultStorePath()
	var events []ladder.Event
	if _, err := os.Stat(storePath); err == nil {
		st, err := ladder.OpenStore(storePath)
		if err != nil {
			return err
		}
		events, err = st.Read()
		if err != nil {
			return err
		}
	}

	now := time.Now().UTC()
	pool, _, err := seatPool("", events, now)
	if err != nil {
		return err
	}

	return withWeaveQueueLock(board, func(q *weaveQueue) error {
		s := findWeaveStory(q, id)
		if s == nil {
			return fmt.Errorf("sprint #%d not found", id)
		}
		sprintUUID := s.UUID
		if sprintUUID == "" {
			sprintUUID = strconv.FormatInt(s.ID, 10)
		}

		seed := panelCaseSeed(sprintUUID, caseID)
		c := ladder.PanelCase{
			ID:            caseID,
			Kind:          kind,
			Points:        ladder.Points(points),
			AuthorVendor:  strings.TrimSpace(authorVendor),
			LowConfidence: lowConfidence,
		}

		wanted := ladder.PanelSize(c)
		members := seatPanel(pool, wanted, c.AuthorVendor)
		if len(members) == 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "capacity wait: no agent available")
			return nil
		}
		if len(members) < wanted {
			seatRecordFallback(s, seatFallbackEvent{Seat: "panel", Story: caseID, WantedBand: 5, ChosenAgent: members[0].Agent, Band: members[0].Band, WantedSize: wanted, Size: len(members)})
		}
		for _, m := range members {
			if m.Band < 5 {
				seatRecordFallback(s, seatFallbackEvent{Seat: "panel", Story: caseID, WantedBand: 5, ChosenAgent: m.Agent, Band: m.Band})
			}
		}

		// Check if planted calibration case under seed
		if ladder.IsPlanted(caseID, seed) {
			plantedPath, err := sprintPlantedPath(s.ID)
			if err != nil {
				return err
			}
			plantedMap, _ := readPlantedFile(plantedPath)
			plantedMap[caseID] = plantedEntry{
				Case:    caseID,
				Planted: true,
				Answer:  strings.TrimSpace(answer),
			}
			if err := savePlantedFile(plantedPath, plantedMap); err != nil {
				return err
			}
		}

		var memberNames []string
		for _, m := range members {
			memberNames = append(memberNames, m.Agent)
		}

		drawEv := panelDrawEvent{
			Case:    caseID,
			Size:    len(members),
			Members: memberNames,
		}
		raw, err := json.Marshal(drawEv)
		if err != nil {
			return err
		}
		weaveStoryAppend(s, weaveConductorName(""), "panel", string(raw))

		_, err = fmt.Fprintf(cmd.OutOrStdout(), "panel %s drawn: size=%d members=%s\n", caseID, len(members), strings.Join(memberNames, ","))
		return err
	})
}

func runSprintPanelVote(cmd *cobra.Command, id int64, flags *weaveOutputFlags, caseID, agent, verdict string, confidence float64, rubric []string) error {
	caseID = strings.TrimSpace(caseID)
	if caseID == "" {
		return fmt.Errorf("--case is required")
	}
	agent = strings.TrimSpace(agent)
	if agent == "" {
		return fmt.Errorf("--agent is required")
	}
	verdict = strings.TrimSpace(verdict)
	if verdict == "" {
		return fmt.Errorf("--verdict is required")
	}
	if confidence < 0 || confidence > 1 {
		return fmt.Errorf("--confidence must be between 0 and 1")
	}

	rubricMap := make(map[string]float64)
	for _, r := range rubric {
		for _, part := range strings.Split(r, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			kv := strings.SplitN(part, "=", 2)
			if len(kv) == 2 {
				val, err := strconv.ParseFloat(strings.TrimSpace(kv[1]), 64)
				if err == nil {
					rubricMap[strings.TrimSpace(kv[0])] = val
				}
			}
		}
	}

	board, err := sprintStoreDir()
	if err != nil {
		return err
	}

	return withWeaveQueueLock(board, func(q *weaveQueue) error {
		s := findWeaveStory(q, id)
		if s == nil {
			return fmt.Errorf("sprint #%d not found", id)
		}

		// Verify agent is a member of the drawn panel for this case
		var drawnMembers []string
		for i := len(s.Thread) - 1; i >= 0; i-- {
			c := s.Thread[i]
			if c.Kind == "panel" {
				var ev panelDrawEvent
				if err := json.Unmarshal([]byte(c.Body), &ev); err == nil && ev.Case == caseID {
					drawnMembers = ev.Members
					break
				}
			}
		}
		if len(drawnMembers) > 0 {
			isMember := false
			for _, m := range drawnMembers {
				if m == agent {
					isMember = true
					break
				}
			}
			if !isMember {
				return fmt.Errorf("agent %s is not a member of panel %s", agent, caseID)
			}
		}

		voteEv := panelVoteEvent{
			Case:       caseID,
			Agent:      agent,
			Verdict:    verdict,
			Confidence: confidence,
			Rubric:     rubricMap,
		}
		raw, err := json.Marshal(voteEv)
		if err != nil {
			return err
		}
		weaveStoryAppend(s, agent, "vote", string(raw))

		_, err = fmt.Fprintf(cmd.OutOrStdout(), "panel %s vote: agent=%s verdict=%s confidence=%.2f\n", caseID, agent, verdict, confidence)
		return err
	})
}

func runSprintPanelVerdict(cmd *cobra.Command, id int64, flags *weaveOutputFlags, caseID string) error {
	caseID = strings.TrimSpace(caseID)
	if caseID == "" {
		return fmt.Errorf("--case is required")
	}

	board, err := sprintStoreDir()
	if err != nil {
		return err
	}

	var (
		res             ladder.VerdictResult
		votesForPlanted []ladder.Vote
	)

	err = withWeaveQueueLock(board, func(q *weaveQueue) error {
		s := findWeaveStory(q, id)
		if s == nil {
			return fmt.Errorf("sprint #%d not found", id)
		}

		// Collect latest vote per agent for caseID
		votesByAgent := make(map[string]ladder.Vote)
		var agentOrder []string
		for _, c := range s.Thread {
			if c.Kind == "vote" || c.Kind == "panel-vote" {
				var v panelVoteEvent
				if err := json.Unmarshal([]byte(c.Body), &v); err == nil && v.Case == caseID {
					if _, ok := votesByAgent[v.Agent]; !ok {
						agentOrder = append(agentOrder, v.Agent)
					}
					votesByAgent[v.Agent] = ladder.Vote{
						Agent:      v.Agent,
						Verdict:    v.Verdict,
						Confidence: v.Confidence,
						Rubric:     v.Rubric,
					}
				}
			}
		}

		var votes []ladder.Vote
		for _, a := range agentOrder {
			votes = append(votes, votesByAgent[a])
		}
		votesForPlanted = votes

		res = ladder.PanelVerdict(votes)
		now := time.Now().UTC()
		season := ladder.SeasonOf(now)

		if res.ToOwner {
			fmt.Fprintln(cmd.OutOrStdout(), "ESCALATE TO OWNER")
			ev := panelVerdictEvent{
				Case:    caseID,
				ToOwner: true,
				Reason:  res.Reason,
				Tally:   res.Tally,
				Season:  season,
			}
			raw, _ := json.Marshal(ev)
			weaveStoryAppend(s, weaveConductorName(""), "escalation", string(raw))
		} else {
			fmt.Fprintf(cmd.OutOrStdout(), "verdict %s: %s\n", caseID, res.Verdict)
			ev := panelVerdictEvent{
				Case:    caseID,
				Verdict: res.Verdict,
				Tally:   res.Tally,
				Season:  season,
			}
			raw, _ := json.Marshal(ev)
			weaveStoryAppend(s, weaveConductorName(""), "verdict", string(raw))
		}
		return nil
	})
	if err != nil {
		return err
	}

	// Check planted cases privately and append judge events
	plantedPath, _ := sprintPlantedPath(id)
	plantedMap, _ := readPlantedFile(plantedPath)
	if entry, ok := plantedMap[caseID]; ok && entry.Planted && entry.Answer != "" {
		now := time.Now().UTC()
		season := ladder.SeasonOf(now)
		store, _ := ladder.OpenStore(ladder.DefaultStorePath())
		for _, v := range votesForPlanted {
			score := 0.0
			if strings.EqualFold(v.Verdict, entry.Answer) {
				score = 1.0
			}
			ev := ladder.Event{
				Schema:   ladder.EventSchema,
				ID:       fmt.Sprintf("planted-%d-%s-%s", id, caseID, v.Agent),
				At:       now,
				Season:   season,
				Kind:     ladder.EventKind("judge"),
				Duty:     ladder.DutyJudge,
				Agent:    v.Agent,
				Sprint:   int(id),
				Score:    score,
				Opponent: ladder.Rating{R: 1500, RD: 50},
				Note:     fmt.Sprintf("planted:%s", caseID),
			}
			if store != nil {
				if err := store.Append(ev); err != nil {
					appendLadderEventDirectly(ladder.DefaultStorePath(), ev)
				}
			} else {
				appendLadderEventDirectly(ladder.DefaultStorePath(), ev)
			}
		}
	}

	return nil
}

func runSprintPanelAudit(cmd *cobra.Command, id int64, flags *weaveOutputFlags, season int, rate float64) error {
	if season <= 0 {
		season = ladder.SeasonOf(time.Now().UTC())
	}
	if rate <= 0 {
		rate = 0.10
	}

	board, err := sprintStoreDir()
	if err != nil {
		return err
	}

	plantedPath, _ := sprintPlantedPath(id)
	plantedMap, _ := readPlantedFile(plantedPath)

	var verdicts []panelVerdictEvent
	err = withWeaveQueueLock(board, func(q *weaveQueue) error {
		s := findWeaveStory(q, id)
		if s == nil {
			return fmt.Errorf("sprint #%d not found", id)
		}

		verdictsByCase := make(map[string]panelVerdictEvent)
		for _, c := range s.Thread {
			if c.Kind == "verdict" || c.Kind == "decision" {
				var ev panelVerdictEvent
				if err := json.Unmarshal([]byte(c.Body), &ev); err == nil && ev.Case != "" {
					if ev.Season == 0 {
						ev.Season = ladder.SeasonOf(c.At)
					}
					verdictsByCase[ev.Case] = ev
				}
			}
		}
		for _, v := range verdictsByCase {
			verdicts = append(verdicts, v)
		}
		return nil
	})
	if err != nil {
		return err
	}

	var sampled []panelVerdictEvent
	for _, v := range verdicts {
		// Non-planted only
		if plantedMap[v.Case].Planted {
			continue
		}
		// Match season
		if v.Season > 0 && v.Season != season {
			continue
		}
		if auditSample(v.Case, season, rate) {
			sampled = append(sampled, v)
		}
	}

	sort.Slice(sampled, func(i, j int) bool {
		return sampled[i].Case < sampled[j].Case
	})

	fmt.Fprintf(cmd.OutOrStdout(), "audit sample (season %d, rate %.2f, %d cases):\n", season, rate, len(sampled))
	for _, v := range sampled {
		fmt.Fprintf(cmd.OutOrStdout(), "  case %s: verdict %s\n", v.Case, v.Verdict)
	}
	return nil
}
