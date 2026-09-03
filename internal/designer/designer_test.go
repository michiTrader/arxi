package designer

import (
	"errors"
	"strings"
	"testing"

	"github.com/michiTrader/arxi/internal/agentstore"
	"github.com/michiTrader/arxi/internal/blueprint"
	"github.com/michiTrader/arxi/internal/kernel"
)

func member(name string) kernel.MemberConfig {
	return kernel.MemberConfig{Name: name, Model: "claude-sonnet-4"}
}

// stored is one entry as agentstore.List would hand it over: loaded, with
// whatever members the file declares.
func stored(file string, ms ...kernel.MemberConfig) agentstore.Entry {
	return agentstore.Entry{
		Name:      file,
		Path:      "agents/" + file + ".yaml",
		Blueprint: &blueprint.Blueprint{Name: file, Config: kernel.Config{Members: ms}},
	}
}

func broken(file string, err error) agentstore.Entry {
	return agentstore.Entry{Name: file, Path: "agents/" + file + ".yaml", Err: err}
}

func TestAFileThatCannotBeAMemberIsListedWithTheReasonAndNotDropped(t *testing.T) {
	cands := Candidates([]agentstore.Entry{
		stored("backend", member("api")),
		broken("half-written", errors.New("members[0]: model is required")),
		stored("empty"),
		stored("platform", member("api"), member("web")),
	})

	if len(cands) != 4 {
		t.Fatalf("4 files in the directory produced %d candidates.\n"+
			"  a directory of four agents where three cannot be members must not look\n"+
			"  like a directory of one, or the operator goes looking for the file they\n"+
			"  know they wrote.", len(cands))
	}
	if cands[0].Unusable != "" {
		t.Errorf("a one-member agent was marked unusable: %q", cands[0].Unusable)
	}
	for i, want := range []string{"", "did not load", "nobody in it", "is itself a team of 2"} {
		if want == "" {
			continue
		}
		if !strings.Contains(cands[i].Unusable, want) {
			t.Errorf("candidate %q says %q; it should say why it cannot be a member (%q)",
				cands[i].Name, cands[i].Unusable, want)
		}
	}
	if !strings.Contains(cands[3].Unusable, "api, web") {
		t.Errorf("the team's refusal is %q; it has to name the members to pick instead,\n"+
			"  because `--members platform` is a dead end and `--members api,web` is not",
			cands[3].Unusable)
	}
}

func TestTheMemberNameAndNotTheFileNameIsWhatCarriesOver(t *testing.T) {
	// `agent create` writes the two equal; a hand edit parts them. Everything
	// downstream addresses a member by the name INSIDE the file -- a stage
	// activation, a watcher's agent:, `run steer <run> <member>` -- so the picker
	// shows the file and composes the member.
	c := Candidates([]agentstore.Entry{stored("backend", member("api"))})[0]
	if c.Name != "backend" {
		t.Errorf("the picker offers %q; it should offer the file name `agent list` prints", c.Name)
	}
	if c.Member.Name != "api" {
		t.Errorf("the composed member is called %q; the file calls it %q", c.Member.Name, "api")
	}
}

func TestALoaderRefusalBecomesOneFrameLine(t *testing.T) {
	// blueprint's errors collect every problem in the file, one per line. A
	// newline inside a frame row does not wrap: it shifts every row below it up
	// and leaves the screen a line short of the height Render was asked for.
	c := Candidates([]agentstore.Entry{
		broken("bad", errors.New("agents/bad.yaml:\n  members[0]: model is required\n  members[1]: name is required")),
	})[0]
	if strings.ContainsAny(c.Unusable, "\n\r") {
		t.Fatalf("the reason still holds a newline: %q", c.Unusable)
	}
	for _, want := range []string{"members[0]: model is required", "members[1]: name is required"} {
		if !strings.Contains(c.Unusable, want) {
			t.Errorf("flattening the loader's error dropped %q from %q", want, c.Unusable)
		}
	}
}

func TestTheTeamIsComposedInThePickOrderAndNotTheDirectoryOrder(t *testing.T) {
	m := New(Candidates([]agentstore.Entry{
		stored("api", member("api")),
		stored("reviewer", member("reviewer")),
		stored("writer", member("writer")),
	}))
	m.Name = newField("release")
	m.Picked = []int{2, 0}
	m.Stages = []string{"draft", "review"}

	team := m.Team()
	if got := memberNames(team.Members); strings.Join(got, ",") != "writer,api" {
		t.Errorf("picked writer then api and got %v.\n"+
			"  the order in the file is the order `agent show` prints, so sorting it\n"+
			"  would discard a choice the operator made with two keystrokes.", got)
	}
	if team.Name != "release" || strings.Join(team.Stages, ",") != "draft,review" {
		t.Errorf("the team under review is %+v, which is not what was typed", team)
	}
}

func TestANameTheStoreHoldsIsTakenEvenWhenTheFileCannotBeAMember(t *testing.T) {
	m := New(Candidates([]agentstore.Entry{
		stored("platform", member("api"), member("web")),
		broken("bad", errors.New("nope")),
	}))
	for _, name := range []string{"platform", "bad"} {
		if m.taken(name) != "agents/"+name+".yaml" {
			t.Errorf("%q is reported free (%q), and CreateTeam will refuse it as existing.\n"+
				"  a team occupies its name however unpickable it is as a member, and the\n"+
				"  refusal has to name the file the operator should go and look at.",
				name, m.taken(name))
		}
	}
	if m.taken("release") != "" {
		t.Errorf("a name no file uses is reported taken, held by %q", m.taken("release"))
	}
}
