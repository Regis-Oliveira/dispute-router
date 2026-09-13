// Package agenttest is the one door past internal/agent's fail-closed flags.
//
// The completer double is internal/llm/llmtest, because a scripted transport
// serves every caller of llm.Completer. What is specific to the representment
// flow is this: a Draft carries an unexported written flag that only the code
// that parsed a model's answer can set, so a test that needs a draft without
// paying for one cannot build it from outside. Production code must never call
// anything here.
package agenttest

import "github.com/regisoliveira/dispute-router/internal/agent"

// Draft is a written agent.Draft with the given recommendation, letter and
// citations, as if the generator had produced it.
//
// A Draft that was never produced by the generator is not one the graders
// will ever see, and agent makes a written one impossible to build from
// outside on purpose. This is the one door through, so a test of the graders
// does not have to script a whole generator call to get a draft to grade.
func Draft(recommendation agent.Recommendation, letter string, cited ...string) agent.Draft {
	if cited == nil {
		cited = []string{}
	}
	return agent.NewWrittenDraftForTest(agent.Draft{
		Recommendation: recommendation,
		Letter:         letter,
		CitedEvidence:  cited,
	})
}
