// Package storetest is the shared conformance suite every store
// implementation must pass. It exercises real SQL
// against a real database, so the tests catch what an in-memory fake never
// would: unique keys, transactional deletes, workspace scoping, join
// correctness.
//
// Point SAG_TEST_DSN at a scratch database to run it. The suite truncates
// the tables it touches before each test, so the database must be
// disposable.
package storetest

import (
	"context"
	"testing"

	"flexie.io/sag/internal/store"
)

// Run executes the full suite against st. newStore is called per subtest so
// each starts from a clean database.
func Run(t *testing.T, st store.Store, reset func(t *testing.T)) {
	t.Helper()
	tests := []struct {
		name string
		fn   func(t *testing.T, st store.Store)
	}{
		{"Workspaces", testWorkspaces},
		{"WorkspaceAdministration", testWorkspaceAdministration},
		{"Users", testUsers},
		{"UserUniqueness", testUserUniqueness},
		{"WorkspaceMembership", testWorkspaceMembership},
		{"GroupMemberMustBelongToWorkspace", testGroupMemberMustBelongToWorkspace},
		{"UserSettings", testUserSettings},
		{"Memory", testMemory},
		{"Groups", testGroups},
		{"GroupMembership", testGroupMembership},
		{"GroupSetForUser", testGroupSetForUser},
		{"Roles", testRoles},
		{"RolePermissionReplacement", testRolePermissionReplacement},
		{"EffectivePermissions", testEffectivePermissions},
		{"CascadingDeletes", testCascadingDeletes},
		{"Sessions", testSessions},
		{"Delegations", testDelegations},
		{"Fleets", testFleets},
		{"FleetOneWinner", testFleetOneWinner},
		{"OverdueFleets", testOverdueFleets},
		{"FleetJoinReads", testFleetJoinReads},
		{"ResolveQueuedParksAtOnce", testResolveQueuedParksAtOnce},
		{"FleetMembersAreWrittenAtOnce", testFleetMembersAreWrittenAtOnce},
		{"JobsAreEnqueuedAtOnce", testJobsAreEnqueuedAtOnce},
		{"FleetRemembersItsModel", testFleetRemembersItsModel},
		{"ParkQueue", testParkQueue},
		{"Jobs", testJobs},
		{"JobReclaim", testJobReclaim},
		{"JobRetryAndDeath", testJobRetryAndDeath},
		{"JobRelease", testJobRelease},
		{"JobSubjectScoping", testJobSubjectScoping},
		{"JobConcurrentClaims", testJobConcurrentClaims},
		{"JobWorkspaceCascade", testJobWorkspaceCascade},
		{"JobHeartbeat", testJobHeartbeat},
		{"JobReclaimDeath", testJobReclaimDeath},
		{"JobLostClaim", testJobLostClaim},
		{"JobLongFailureReason", testJobLongFailureReason},
		{"JobPrefixIsNotAPattern", testJobPrefixIsNotAPattern},
		{"JobEmptyPayload", testJobEmptyPayload},
		{"JobEnqueueForcesPending", testJobEnqueueForcesPending},
		{"Vendors", testVendors},
		{"VendorCredentialUpdate", testVendorCredentialUpdate},
		{"VendorDeleteGuard", testVendorDeleteGuard},
		{"AIModels", testAIModels},
		{"ChatList", testChatList},
		{"ChatSearch", testChatSearch},
		{"ChatManagement", testChatManagement},
		{"Steps", testSteps},
		{"StepCheckpointing", testStepCheckpointing},
		{"NextSeqIsAtomic", testNextSeqIsAtomic},
		{"ConcurrentStepsKeepTheirAttribution", testConcurrentStepsKeepTheirAttribution},
		{"ResolveToolCallKnowsItsRow", testResolveToolCallKnowsItsRow},
		{"ToolCallApprovalIsSticky", testToolCallApprovalIsSticky},
		{"InterruptedToolCallsAreClosedOut", testInterruptedToolCallsAreClosedOut},
		{"ConversationCascade", testConversationCascade},
		{"WorkspaceCascade", testWorkspaceCascade},
		{"DeletingAnAgentKeepsItsConversations", testDeletingAnAgentDoesNotDeleteItsConversations},
		{"OrphansCannotBeCreated", testAnOrphanCannotBeCreated},
		{"BrainTree", testBrainTree},
		{"BrainDocumentByTitle", testBrainDocumentByTitle},
		{"BrainGraphIsSymmetric", testBrainGraphIsSymmetric},
		{"BrainGraphCannotLeaveItsBrain", testBrainGraphCannotLeaveItsBrain},
		{"ADocumentCannotMoveBetweenBrains", testADocumentCannotMoveBetweenBrains},
		{"BrainSearch", testBrainSearch},
		{"AgentBrains", testAgentBrains},
		{"ToolSync", testToolSync},
		{"MCPServers", testMCPServers},
		{"MCPToolProjection", testMCPToolProjection},
		{"ToolGrants", testToolGrants},
		{"Agents", testAgents},
		{"WorkflowVersions", testWorkflowVersions},
		{"WorkflowAssignmentReplacement", testWorkflowAssignmentReplacement},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			reset(t)
			tc.fn(t, st)
		})
	}
}

func ctx() context.Context { return context.Background() }
