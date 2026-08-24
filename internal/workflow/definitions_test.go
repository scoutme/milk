package workflow

import "testing"

func TestDefinition_Validate_MissingName(t *testing.T) {
	d := Definition{Stages: []Stage{{ID: "a", Kind: StageKindAgentTurn, Role: "r", Prompt: "p"}}}
	if err := d.Validate(); err == nil {
		t.Error("expected error for missing name")
	}
}

func TestDefinition_Validate_NoStages(t *testing.T) {
	d := Definition{Name: "x"}
	if err := d.Validate(); err == nil {
		t.Error("expected error for no stages")
	}
}

func TestDefinition_Validate_AgentTurnMissingRoleOrPrompt(t *testing.T) {
	cases := []Stage{
		{ID: "a", Kind: StageKindAgentTurn, Prompt: "p"},
		{ID: "a", Kind: StageKindAgentTurn, Role: "r"},
	}
	for _, s := range cases {
		d := Definition{Name: "x", Stages: []Stage{s}}
		if err := d.Validate(); err == nil {
			t.Errorf("expected error for stage %+v", s)
		}
	}
}

func TestDefinition_Validate_LoopNeedsOverOrMaxIterations(t *testing.T) {
	d := Definition{Name: "x", Stages: []Stage{{
		ID: "loop1", Kind: StageKindLoop,
		Body: []Stage{{ID: "a", Kind: StageKindAgentTurn, Role: "r", Prompt: "p"}},
	}}}
	if err := d.Validate(); err == nil {
		t.Error("expected error: loop with neither over nor max_iterations")
	}
}

func TestDefinition_Validate_BoundedLoopRequiresVerdictOnLastBodyStage(t *testing.T) {
	d := Definition{Name: "x", Stages: []Stage{{
		ID: "loop1", Kind: StageKindLoop, MaxIterations: 3,
		Body: []Stage{{ID: "a", Kind: StageKindAgentTurn, Role: "r", Prompt: "p"}},
	}}}
	if err := d.Validate(); err == nil {
		t.Error("expected error: max_iterations-bounded loop body missing verdict rules")
	}
}

func TestDefinition_Validate_ValidBoundedLoop(t *testing.T) {
	d := Definition{Name: "x", Stages: []Stage{{
		ID: "loop1", Kind: StageKindLoop, MaxIterations: 3,
		Body: []Stage{{
			ID: "a", Kind: StageKindAgentTurn, Role: "r", Prompt: "p",
			Verdict: map[string]VerdictRule{"good_to_go": {Action: "break"}},
		}},
	}}}
	if err := d.Validate(); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestDefinition_Validate_ParallelGroupNeedsOver(t *testing.T) {
	d := Definition{Name: "x", Stages: []Stage{{
		ID: "pg", Kind: StageKindParallelGroup,
		Body: []Stage{{ID: "a", Kind: StageKindAgentTurn, Role: "r", Prompt: "p"}},
	}}}
	if err := d.Validate(); err == nil {
		t.Error("expected error: parallel_group missing over")
	}
}

func TestDefinition_Validate_UnknownKind(t *testing.T) {
	d := Definition{Name: "x", Stages: []Stage{{ID: "a", Kind: "not_a_kind"}}}
	if err := d.Validate(); err == nil {
		t.Error("expected error for unknown stage kind")
	}
}

func TestDefinition_Validate_MissingID(t *testing.T) {
	d := Definition{Name: "x", Stages: []Stage{{Kind: StageKindAgentTurn, Role: "r", Prompt: "p"}}}
	if err := d.Validate(); err == nil {
		t.Error("expected error for stage missing id")
	}
}
