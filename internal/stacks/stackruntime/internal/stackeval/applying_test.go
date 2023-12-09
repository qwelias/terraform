package stackeval

import (
	"context"
	"slices"
	"testing"

	"github.com/davecgh/go-spew/spew"
	"github.com/google/go-cmp/cmp"
	"github.com/zclconf/go-cty/cty"

	"github.com/hashicorp/terraform/internal/addrs"
	"github.com/hashicorp/terraform/internal/collections"
	"github.com/hashicorp/terraform/internal/configs/configschema"
	"github.com/hashicorp/terraform/internal/plans"
	"github.com/hashicorp/terraform/internal/promising"
	"github.com/hashicorp/terraform/internal/providers"
	"github.com/hashicorp/terraform/internal/stacks/stackaddrs"
	"github.com/hashicorp/terraform/internal/stacks/stackplan"
	"github.com/hashicorp/terraform/internal/stacks/stackstate"
	"github.com/hashicorp/terraform/internal/terraform"
)

func TestApply_componentOrdering(t *testing.T) {
	// This verifies that component instances have their plans applied in a
	// suitable order during the apply phase, both for normal plans and for
	// destroy plans.
	//
	// This test also creates a plan using the normal planning logic, so
	// it partially acts as an integration test for planning and applying
	// with component inter-dependencies (since the plan phase is the one
	// responsible for actually calculating the dependencies.)
	//
	// Since this is testing some concurrent code, the test might produce
	// false-positives if things just happen to occur in the right order
	// despite the sequencing code being incorrect. Consider running this
	// test under the Go data race detector to find memory-safety-related
	// problems, but also keep in mind that not all sequencing problems are
	// caused by data races.
	//
	// If this test seems to be flaking and the race detector doesn't dig up
	// any clues, you might consider the following:
	//  - Is the code in function ApplyPlan waiting for all of the prerequisites
	//    captured in the plan? Is it honoring the reversed order expected
	//    for destroy plans?
	//  - Is the ChangeExec function, and its subsequent execution, correctly
	//    scheduling all of the apply tasks that were registered?
	//
	// If other tests in this package (or that call into this package) are
	// also consistently failing, it'd likely be more productive to debug and
	// fix those first, which might then give a clue as to what's making this
	// test misbehave.

	cfg := testStackConfig(t, "applying", "component_dependencies")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	testProviderAddr := addrs.NewBuiltInProvider("test")
	testProviderSchema := providers.GetProviderSchemaResponse{
		ResourceTypes: map[string]providers.Schema{
			"test_report": {
				Block: &configschema.Block{
					Attributes: map[string]*configschema.Attribute{
						"marker": {
							Type:     cty.String,
							Required: true,
						},
					},
				},
			},
		},
	}

	cmpAAddr := stackaddrs.AbsComponent{
		Stack: stackaddrs.RootStackInstance,
		Item: stackaddrs.Component{
			Name: "a",
		},
	}
	cmpAInstAddr := stackaddrs.AbsComponentInstance{
		Stack: cmpAAddr.Stack,
		Item: stackaddrs.ComponentInstance{
			Component: cmpAAddr.Item,
			Key:       addrs.NoKey,
		},
	}
	cmpBAddr := stackaddrs.AbsComponent{
		Stack: stackaddrs.RootStackInstance,
		Item: stackaddrs.Component{
			Name: "b",
		},
	}
	cmpBInst1Addr := stackaddrs.AbsComponentInstance{
		Stack: cmpBAddr.Stack,
		Item: stackaddrs.ComponentInstance{
			Component: cmpBAddr.Item,
			Key:       addrs.StringKey("i"),
		},
	}
	cmpBInst2Addr := stackaddrs.AbsComponentInstance{
		Stack: cmpBAddr.Stack,
		Item: stackaddrs.ComponentInstance{
			Component: cmpBAddr.Item,
			Key:       addrs.StringKey("ii"),
		},
	}
	cmpBInst3Addr := stackaddrs.AbsComponentInstance{
		Stack: cmpBAddr.Stack,
		Item: stackaddrs.ComponentInstance{
			Component: cmpBAddr.Item,
			Key:       addrs.StringKey("iii"),
		},
	}
	cmpCAddr := stackaddrs.AbsComponent{
		Stack: stackaddrs.RootStackInstance,
		Item: stackaddrs.Component{
			Name: "c",
		},
	}
	cmpCInstAddr := stackaddrs.AbsComponentInstance{
		Stack: cmpCAddr.Stack,
		Item: stackaddrs.ComponentInstance{
			Component: cmpCAddr.Item,
			Key:       addrs.NoKey,
		},
	}

	// First we need to create a plan for this configuration, which will
	// include the calculated component dependencies.
	plan, err := promising.MainTask(ctx, func(ctx context.Context) (*stackplan.Plan, error) {
		main := NewForPlanning(cfg, stackstate.NewState(), PlanOpts{
			PlanningMode: plans.NormalMode,
			ProviderFactories: ProviderFactories{
				testProviderAddr: func() (providers.Interface, error) {
					return &terraform.MockProvider{
						GetProviderSchemaResponse: &testProviderSchema,
						PlanResourceChangeFn: func(prcr providers.PlanResourceChangeRequest) providers.PlanResourceChangeResponse {
							return providers.PlanResourceChangeResponse{
								PlannedState: prcr.ProposedNewState,
							}
						},
					}, nil
				},
			},
		})

		plan, diags := testPlan(t, main)
		assertNoDiagnostics(t, diags)
		return plan, nil
	})
	if err != nil {
		t.Fatalf("planning failed: %s", err)
	}

	// Before we proceed further we'll check that the plan contains the
	// expected dependency relationships, because missing dependency edges
	// will make the following tests invalid, and testing this is not
	// subject to concurrency-related false-positives.
	//
	// This is not comprehensive, because the dependency calculation logic
	// should already be tested more completely elsewhere. If this part fails
	// then hopefully at least one of the planning-specific tests is also
	// failing, and will give some more clues as to what's gone wrong here.
	{
		cmpPlan := plan.Components.Get(cmpCInstAddr)
		gotDeps := cmpPlan.Dependencies
		wantDeps := collections.NewSet[stackaddrs.AbsComponent]()
		wantDeps.Add(cmpBAddr)
		if diff := cmp.Diff(wantDeps, gotDeps, collections.CmpOptions); diff != "" {
			t.Fatalf("wrong dependencies for component.c\n%s", diff)
		}
	}
	{
		cmpPlan := plan.Components.Get(cmpBInst1Addr)
		gotDeps := cmpPlan.Dependencies
		wantDeps := collections.NewSet[stackaddrs.AbsComponent]()
		wantDeps.Add(cmpAAddr)
		if diff := cmp.Diff(wantDeps, gotDeps, collections.CmpOptions); diff != "" {
			t.Fatalf("wrong dependencies for component.b[\"i\"]\n%s", diff)
		}
	}
}

func sliceElementsInRelativeOrder[S ~[]E, E comparable](s S, v1, v2 E) bool {
	idx1 := slices.Index(s, v1)
	idx2 := slices.Index(s, v2)
	if idx1 < 0 || idx2 < 0 {
		// both values must actually be present for this test to be meaningful
		return false
	}
	return idx1 < idx2
}

func assertSliceElementsInRelativeOrder[S ~[]E, E comparable](t *testing.T, s S, v1, v2 E) {
	t.Helper()

	if !sliceElementsInRelativeOrder(s, v1, v2) {
		t.Fatalf("incorrect element order\ngot: %s\nwant: %#v before %#v", spew.Sdump(s), v1, v2)
	}
}
