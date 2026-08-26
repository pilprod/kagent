package controller

import (
	"context"
	"errors"
	"testing"

	atev1alpha1 "github.com/agent-substrate/substrate/pkg/api/v1alpha1"
	atefake "github.com/agent-substrate/substrate/pkg/client/clientset/versioned/fake"
	dbpkg "github.com/kagent-dev/kagent/go/api/database"
	kagentv1alpha3 "github.com/kagent-dev/kagent/go/api/v1alpha3"
	v2translator "github.com/kagent-dev/kagent/go/core/v2/translator"
	"istio.io/istio/pkg/kube/krt"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestReconcilerPersistsPairInOrder(t *testing.T) {
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	opts := krt.NewOptionsBuilder(stop, "test", nil)
	template := &kagentv1alpha3.AgentTemplate{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "assistant", UID: "template-uid", Generation: 3}}
	harness := &kagentv1alpha3.Harness{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "kagent", UID: "harness-uid"}}
	desiredActor := &atev1alpha1.ActorTemplate{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "assistant-kagent-revision"}}
	revision := &v2translator.Revision{BackendKind: dbpkg.RuntimeBackendKindSubstrate}
	revisionID, err := revision.Digest()
	if err != nil {
		t.Fatal(err)
	}
	state := PairReconciliation{
		Pair:     AgentTemplateHarnessPair{AgentTemplate: template, Harness: harness},
		Revision: revision, RevisionID: revisionID, DesiredActorTemplate: desiredActor,
	}
	reconciliations := krt.NewStaticCollection(nil, []PairReconciliation{state}, opts.WithName("Reconciliations")...)
	status := kagentv1alpha3.AgentTemplateStatus{ObservedGeneration: 1, Harnesses: []kagentv1alpha3.AgentTemplateHarnessStatus{{
		Harness: "kagent", Conditions: []metav1.Condition{{Type: kagentv1alpha3.AgentTemplateConditionReady, Status: metav1.ConditionFalse}},
	}}}
	statuses := krt.NewStaticCollection(nil, []krt.ObjectWithStatus[*kagentv1alpha3.AgentTemplate, kagentv1alpha3.AgentTemplateStatus]{{Obj: template, Status: status}}, opts.WithName("Statuses")...)
	store := &fakeRuntimeRevisionStore{}
	// Substrate does not generate apply configurations, so its suggested
	// NewClientset replacement is unavailable.
	actors := atefake.NewSimpleClientset().ApiV1alpha1() //nolint:staticcheck
	var statusWrite *kagentv1alpha3.AgentTemplate
	reconciler := &Reconciler{
		collections: Collections{
			AgentTemplates:  krt.NewStaticCollection(nil, []*kagentv1alpha3.AgentTemplate{template}, opts.WithName("AgentTemplates")...),
			Reconciliations: reconciliations, AgentTemplateStatuses: statuses,
		},
		actors: actors, store: store,
		updateStatus: func(_ context.Context, template *kagentv1alpha3.AgentTemplate) error {
			statusWrite = template
			return nil
		},
	}

	if err := reconciler.reconcilePair(context.Background(), state.ResourceName()); err != nil {
		t.Fatal(err)
	}
	if store.pair == nil {
		t.Fatal("pair was not stored")
	}
	created, err := actors.ActorTemplates("team-a").Get(context.Background(), desiredActor.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("ActorTemplate was not created: %v", err)
	}
	if store.revision != nil {
		t.Fatal("revision was stored before its ActorTemplate was observed")
	}

	observed := created.DeepCopy()
	observed.UID = "actor-uid"
	observed.Status.Phase = atev1alpha1.PhaseReady
	state.ObservedActorTemplate = observed
	reconciliations.UpdateObject(state)
	if err := reconciler.reconcilePair(context.Background(), state.ResourceName()); err != nil {
		t.Fatal(err)
	}
	if store.revision == nil || !store.markedSuccessful {
		t.Fatal("ready revision was not stored and marked successful")
	}

	if err := reconciler.reconcileStatus(context.Background(), "team-a/assistant"); err != nil {
		t.Fatal(err)
	}
	if statusWrite == nil || statusWrite.Status.Harnesses[0].Conditions[0].LastTransitionTime.IsZero() {
		t.Fatal("desired status was not written with a transition time")
	}

	reconciliations.DeleteObject(state.ResourceName())
	if err := reconciler.reconcilePair(context.Background(), state.ResourceName()); err != nil {
		t.Fatal(err)
	}
	if store.retired != state.ResourceName() {
		t.Fatalf("retired pair = %q, want %q", store.retired, state.ResourceName())
	}
}

func TestReconcilerPersistsExternalRevisionWithoutActorTemplate(t *testing.T) {
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	opts := krt.NewOptionsBuilder(stop, "test", nil)
	template := &kagentv1alpha3.AgentTemplate{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "assistant", UID: "template-uid", Generation: 3}}
	harness := &kagentv1alpha3.Harness{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "codex", UID: "harness-uid"}}
	revision := &v2translator.Revision{
		Namespace: "team-a", AgentTemplateName: "assistant", HarnessName: "codex",
		BackendKind: dbpkg.RuntimeBackendKindExternal, ExternalRuntime: dbpkg.ExternalRuntimeCodex,
		ExternalProfile: []byte(`{"version":"v1","instruction":"help","tools":[]}`),
		AgentCardJSON:   []byte(`{"name":"assistant","version":"v1"}`),
		Provenance:      []byte(`{"template":"template-uid"}`),
	}
	revisionID, err := revision.Digest()
	if err != nil {
		t.Fatal(err)
	}
	state := PairReconciliation{
		Pair: AgentTemplateHarnessPair{AgentTemplate: template, Harness: harness}, Revision: revision, RevisionID: revisionID,
	}
	reconciliations := krt.NewStaticCollection(nil, []PairReconciliation{state}, opts.WithName("ExternalReconciliations")...)
	store := &fakeRuntimeRevisionStore{}
	actors := atefake.NewSimpleClientset().ApiV1alpha1() //nolint:staticcheck
	var statusWrite *kagentv1alpha3.AgentTemplate
	reconciler := &Reconciler{
		collections: Collections{
			AgentTemplates:  krt.NewStaticCollection(nil, []*kagentv1alpha3.AgentTemplate{template}, opts.WithName("ExternalAgentTemplates")...),
			Reconciliations: reconciliations,
		},
		actors: actors, store: store,
		updateStatus: func(_ context.Context, template *kagentv1alpha3.AgentTemplate) error {
			statusWrite = template
			return nil
		},
	}

	if err := reconciler.reconcilePair(t.Context(), state.ResourceName()); err != nil {
		t.Fatal(err)
	}
	if store.pair == nil || store.revision == nil || !store.markedSuccessful {
		t.Fatalf("external revision was not persisted and marked ready: pair=%v revision=%v successful=%v", store.pair != nil, store.revision != nil, store.markedSuccessful)
	}
	if store.revision.BackendKind != dbpkg.RuntimeBackendKindExternal || store.revision.ExternalRuntime != dbpkg.ExternalRuntimeCodex {
		t.Fatalf("unexpected external backend identity: %+v", store.revision)
	}
	if string(store.revision.ExternalProfile) != string(revision.ExternalProfile) || store.revision.ActorTemplateNamespace != "" || store.revision.ActorTemplateName != "" {
		t.Fatalf("external revision used cluster compute identity: %+v", store.revision)
	}
	actorList, err := actors.ActorTemplates("team-a").List(t.Context(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(actorList.Items) != 0 {
		t.Fatalf("external revision created %d ActorTemplates", len(actorList.Items))
	}

	pending := statusForPair(state, 3, "")
	if pending.LatestSuccessfulRevision != "" {
		t.Fatalf("external status promoted before persistence: %q", pending.LatestSuccessfulRevision)
	}
	if statusWrite == nil || len(statusWrite.Status.Harnesses) != 1 || statusWrite.Status.Harnesses[0].LatestSuccessfulRevision != revisionID.String() {
		t.Fatalf("persisted external revision was not promoted in status: %+v", statusWrite)
	}
	ready := false
	for _, condition := range statusWrite.Status.Harnesses[0].Conditions {
		if condition.Type == kagentv1alpha3.AgentTemplateConditionReady {
			ready = condition.Status == metav1.ConditionTrue && condition.Reason == "ExternalRuntimePrepared"
		}
	}
	if !ready {
		t.Fatalf("external revision was not reported prepared: %+v", statusWrite.Status.Harnesses[0].Conditions)
	}
}

func TestExternalRevisionStatusIsNotPromotedWhenPersistenceFails(t *testing.T) {
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	opts := krt.NewOptionsBuilder(stop, "test", nil)
	template := &kagentv1alpha3.AgentTemplate{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "assistant", UID: "template-uid", Generation: 4},
		Status: kagentv1alpha3.AgentTemplateStatus{Harnesses: []kagentv1alpha3.AgentTemplateHarnessStatus{{
			Harness: "codex", LatestSuccessfulRevision: "previous-revision",
		}}},
	}
	harness := &kagentv1alpha3.Harness{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "codex", UID: "harness-uid"}}
	revision := &v2translator.Revision{
		BackendKind: dbpkg.RuntimeBackendKindExternal, ExternalRuntime: dbpkg.ExternalRuntimeCodex,
		ExternalProfile: []byte(`{"version":"v1","instruction":"help","tools":[]}`),
		AgentCardJSON:   []byte(`{"name":"assistant","version":"v1"}`), Provenance: []byte(`{}`),
		EgressDestinations: []string{},
	}
	revisionID, err := revision.Digest()
	if err != nil {
		t.Fatal(err)
	}
	state := PairReconciliation{Pair: AgentTemplateHarnessPair{AgentTemplate: template, Harness: harness}, Revision: revision, RevisionID: revisionID}
	reconciliations := krt.NewStaticCollection(nil, []PairReconciliation{state}, opts.WithName("FailingExternalReconciliations")...)
	store := &fakeRuntimeRevisionStore{markErr: errors.New("database unavailable")}
	statusWrites := 0
	reconciler := &Reconciler{
		collections: Collections{
			AgentTemplates:  krt.NewStaticCollection(nil, []*kagentv1alpha3.AgentTemplate{template}, opts.WithName("FailingExternalAgentTemplates")...),
			Reconciliations: reconciliations,
		},
		actors: atefake.NewSimpleClientset().ApiV1alpha1(), store: store, //nolint:staticcheck
		updateStatus: func(context.Context, *kagentv1alpha3.AgentTemplate) error { statusWrites++; return nil },
	}

	err = reconciler.reconcilePair(t.Context(), state.ResourceName())
	if err == nil || !errors.Is(err, store.markErr) {
		t.Fatalf("reconcile error = %v, want persistence failure", err)
	}
	if statusWrites != 0 {
		t.Fatalf("status was promoted %d times after persistence failed", statusWrites)
	}
	pending := statusForPair(state, template.Generation, template.Status.Harnesses[0].LatestSuccessfulRevision)
	if pending.LatestSuccessfulRevision != "previous-revision" {
		t.Fatalf("pending status lost previous successful revision: %+v", pending)
	}
}

func TestReconcileStatusDoesNotOverwriteAcknowledgementFromStaleDerivation(t *testing.T) {
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	opts := krt.NewOptionsBuilder(stop, "test", nil)

	current := &kagentv1alpha3.AgentTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "team-a", Name: "assistant", UID: "template-uid", Generation: 4, ResourceVersion: "12",
		},
		Status: kagentv1alpha3.AgentTemplateStatus{
			ObservedGeneration: 4,
			Harnesses: []kagentv1alpha3.AgentTemplateHarnessStatus{{
				Harness: "codex", DesiredRevision: "revision-4", LatestSuccessfulRevision: "revision-4",
				Conditions: []metav1.Condition{{
					Type: kagentv1alpha3.AgentTemplateConditionReady, Status: metav1.ConditionTrue,
					Reason: "ExternalRuntimePrepared", ObservedGeneration: 4,
				}},
			}},
		},
	}
	staleSource := current.DeepCopy()
	staleSource.ResourceVersion = "11"
	staleSource.Status = kagentv1alpha3.AgentTemplateStatus{}
	staleDerived := kagentv1alpha3.AgentTemplateStatus{
		ObservedGeneration: 4,
		Harnesses: []kagentv1alpha3.AgentTemplateHarnessStatus{{
			Harness: "codex", DesiredRevision: "revision-4",
			Conditions: []metav1.Condition{{
				Type: kagentv1alpha3.AgentTemplateConditionReady, Status: metav1.ConditionFalse,
				Reason: "ExternalRevisionPending", ObservedGeneration: 4,
			}},
		}},
	}
	statuses := krt.NewStaticCollection(nil, []krt.ObjectWithStatus[*kagentv1alpha3.AgentTemplate, kagentv1alpha3.AgentTemplateStatus]{
		{Obj: staleSource, Status: staleDerived},
	}, opts.WithName("StaleDerivedStatuses")...)
	writes := 0
	reconciler := &Reconciler{
		collections: Collections{
			AgentTemplates:        krt.NewStaticCollection(nil, []*kagentv1alpha3.AgentTemplate{current}, opts.WithName("AcknowledgedAgentTemplates")...),
			AgentTemplateStatuses: statuses,
		},
		updateStatus: func(context.Context, *kagentv1alpha3.AgentTemplate) error {
			writes++
			return nil
		},
	}

	if err := reconciler.reconcileStatus(t.Context(), "team-a/assistant"); err != nil {
		t.Fatal(err)
	}
	if writes != 0 {
		t.Fatalf("stale derived status overwrote acknowledged status with %d write(s)", writes)
	}
}

func TestPromotePairStatusRejectsSupersededIdentityGenerationAndRevision(t *testing.T) {
	revision := &v2translator.Revision{
		BackendKind: dbpkg.RuntimeBackendKindExternal, ExternalRuntime: dbpkg.ExternalRuntimeCodex,
		ExternalProfile: []byte(`{"version":"v1","instruction":"first","tools":[]}`),
	}
	revisionID, err := revision.Digest()
	if err != nil {
		t.Fatal(err)
	}
	newerRevision := &v2translator.Revision{
		BackendKind: dbpkg.RuntimeBackendKindExternal, ExternalRuntime: dbpkg.ExternalRuntimeCodex,
		ExternalProfile: []byte(`{"version":"v1","instruction":"second","tools":[]}`),
	}
	newerRevisionID, err := newerRevision.Digest()
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		change func(*kagentv1alpha3.AgentTemplate, *kagentv1alpha3.Harness, *PairReconciliation)
	}{
		{
			name: "template UID",
			change: func(template *kagentv1alpha3.AgentTemplate, _ *kagentv1alpha3.Harness, state *PairReconciliation) {
				template.UID = "replacement-template-uid"
				state.Pair.AgentTemplate = template
			},
		},
		{
			name: "template generation",
			change: func(template *kagentv1alpha3.AgentTemplate, _ *kagentv1alpha3.Harness, state *PairReconciliation) {
				template.Generation++
				state.Pair.AgentTemplate = template
			},
		},
		{
			name: "harness UID",
			change: func(_ *kagentv1alpha3.AgentTemplate, harness *kagentv1alpha3.Harness, state *PairReconciliation) {
				harness.UID = "replacement-harness-uid"
				state.Pair.Harness = harness
			},
		},
		{
			name: "desired revision",
			change: func(_ *kagentv1alpha3.AgentTemplate, _ *kagentv1alpha3.Harness, state *PairReconciliation) {
				state.Revision = newerRevision
				state.RevisionID = newerRevisionID
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stop := make(chan struct{})
			t.Cleanup(func() { close(stop) })
			opts := krt.NewOptionsBuilder(stop, "test", nil)
			capturedTemplate := &kagentv1alpha3.AgentTemplate{ObjectMeta: metav1.ObjectMeta{
				Namespace: "team-a", Name: "assistant", UID: "template-uid", Generation: 7, ResourceVersion: "20",
			}}
			capturedHarness := &kagentv1alpha3.Harness{ObjectMeta: metav1.ObjectMeta{
				Namespace: "team-a", Name: "codex", UID: "harness-uid", Generation: 2,
			}}
			captured := PairReconciliation{
				Pair:     AgentTemplateHarnessPair{AgentTemplate: capturedTemplate, Harness: capturedHarness},
				Revision: revision, RevisionID: revisionID,
			}
			currentTemplate := capturedTemplate.DeepCopy()
			currentHarness := capturedHarness.DeepCopy()
			current := captured
			current.Pair = AgentTemplateHarnessPair{AgentTemplate: currentTemplate, Harness: currentHarness}
			tt.change(currentTemplate, currentHarness, &current)

			reconciliations := krt.NewStaticCollection(nil, []PairReconciliation{current}, opts.WithName("CurrentReconciliations")...)
			writes := 0
			reconciler := &Reconciler{
				collections: Collections{
					AgentTemplates:  krt.NewStaticCollection(nil, []*kagentv1alpha3.AgentTemplate{currentTemplate}, opts.WithName("CurrentAgentTemplates")...),
					Reconciliations: reconciliations,
				},
				updateStatus: func(context.Context, *kagentv1alpha3.AgentTemplate) error {
					writes++
					return nil
				},
			}

			if err := reconciler.promotePairStatus(t.Context(), &captured); err != nil {
				t.Fatal(err)
			}
			if writes != 0 {
				t.Fatalf("superseded %s produced %d status write(s)", tt.name, writes)
			}
		})
	}
}

func TestCleanupExternalRevisionIsDatabaseOnly(t *testing.T) {
	store := &fakeRuntimeRevisionStore{unreferenced: []dbpkg.RuntimeRevision{{
		Revision: "external-revision", BackendKind: dbpkg.RuntimeBackendKindExternal,
		ExternalRuntime: dbpkg.ExternalRuntimeCodex, ExternalProfile: []byte(`{"version":"v1"}`), Phase: "Ready",
	}}}
	actors := atefake.NewSimpleClientset().ApiV1alpha1() //nolint:staticcheck
	reconciler := &Reconciler{actors: actors, store: store}
	if err := reconciler.cleanupUnreferencedRevisions(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(store.deleted) != 1 || store.deleted[0] != "external-revision" {
		t.Fatalf("external revision delete calls = %v", store.deleted)
	}
}

type fakeRuntimeRevisionStore struct {
	pair             *dbpkg.AgentTemplateHarnessPair
	revision         *dbpkg.RuntimeRevision
	markedSuccessful bool
	retired          string
	unreferenced     []dbpkg.RuntimeRevision
	deleted          []string
	markErr          error
}

func (s *fakeRuntimeRevisionStore) UpsertAgentTemplateHarnessPair(_ context.Context, pair dbpkg.AgentTemplateHarnessPair) error {
	s.pair = &pair
	return nil
}

func (s *fakeRuntimeRevisionStore) UpsertRuntimeRevision(_ context.Context, revision dbpkg.RuntimeRevision) error {
	s.revision = &revision
	return nil
}

func (s *fakeRuntimeRevisionStore) MarkRuntimeRevisionSuccessful(context.Context, dbpkg.AgentTemplateHarnessPair) error {
	if s.markErr != nil {
		return s.markErr
	}
	s.markedSuccessful = true
	return nil
}

func (s *fakeRuntimeRevisionStore) RetireAgentTemplateHarnessPair(_ context.Context, namespace, template, harness string) error {
	s.retired = namespace + "/" + template + "/" + harness
	return nil
}

func (s *fakeRuntimeRevisionStore) ListUnreferencedRuntimeRevisions(context.Context) ([]dbpkg.RuntimeRevision, error) {
	return append([]dbpkg.RuntimeRevision(nil), s.unreferenced...), nil
}

func (s *fakeRuntimeRevisionStore) DeleteUnreferencedRuntimeRevision(_ context.Context, revision string) error {
	s.deleted = append(s.deleted, revision)
	return nil
}
