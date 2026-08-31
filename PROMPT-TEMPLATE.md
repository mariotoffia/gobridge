Implement chunk <Cn> of PROD_READY_PLAN.md.
Follow that chunk's Goal, Work, Proof, and Exit gate exactly.

Read first: AGENTS.md and {ARCHITECTURE,DDD,UBIQUITOUS}.md, the ADRs.

TDD. Unit, integration and benchmark tests per AGENTS.md, in <package>_test
packages, reusing testutils and existing harnesses rather than
new harnesses. Then run an adversarial review of the implementation
and fix until bug free.