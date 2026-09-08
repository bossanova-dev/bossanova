# Premise adjudication

Use this recipe to grade a review finding before its remedy is applied, declined, or published. The
adjudicated unit is a **premise**, never a whole finding: a finding is a bundle of separable claims,
and a single accept/reject verdict over the bundle is either a wrong accept or a wrong reject the
moment its parts disagree. The shared checklist is mandatory and ordered.

Companion recipe: when a premise turns on whether a gate, guard, or assertion is load-bearing,
settle it with a falsification probe rather than by reasoning from the gate's literal.

## Shared checklist

1. **Decompose before any verdict.** Enumerate the finding's premises before grading anything. A
   premise is each distinct factual claim the finding makes about the tree, each link of a causal
   chain it asserts, and each separable part of the remedy it proposes. A finding with N
   load-bearing premises yields N verdicts, not one. Write the enumeration down: a decomposition
   held only in the reader's head is indistinguishable afterwards from a wholesale verdict.
2. **One verdict per premise, each carrying its evidence.** The verdict vocabulary is exactly
   `held`, `refuted`, `unverified`. Evidence is the source substring read, or the command run and
   its result — something a later reader can grep for. A bare line number is not evidence: line
   numbers move, and a coordinate cannot be re-checked against a tree that has since changed.
3. **Absence is `unverified`, never `held`.** A premise with no record, an unreadable record, or a
   record whose verdict is outside the vocabulary reads as unverified. Never infer that a premise
   holds from a status, a severity, a completed upstream ticket, a bot's confidence, a capped review
   verdict, or a prior round's justification prose. None of those is a read of the code the premise
   is about; a gate that accepts them is a vacuous gate, green on exactly the values it exists to
   reject.
4. **Check a cited contract's consumers.** When a premise asserts that an in-tree registry,
   constant, or contract _binds_ something, grep for what reads it before accepting the premise. A
   definition that occurs exactly once — its own declaration — is a taxonomy, not an enforced
   contract, and a finding resting on it is asserting an obligation nothing imposes.
5. **The gate.** No premise may be `unverified` when the remedy is
   **applied**, **declined**, or **published**.
   Publication counts because a published claim enters the pull-request record and
   the follow-up-ticket prompt, where people and later agents act on it; an advisory response is a
   publication for this purpose even though it opens no fix cycle. On the decline path the gate has
   one further obligation: when a premise is refuted **because in-tree prose asserted it**, correct
   that prose in the same pass. The sentence that made the reviewer's reading reasonable is what
   re-seeds the identical finding next round, and leaving it standing manufactures a self-inflicted
   finding.
6. **Verdicts are not sticky.** A new round re-derives each premise from source rather than checking
   that the old finding's text is gone. A correcting edit is itself an unreviewed claim, so a
   verdict recorded by the same context that authored the fix is not independent evidence; the
   confirming pass must read the code, in a context that did not write it.

## Grading a multi-part remedy

Remedies decompose on the same rule as claims. Affirm the parts that hold, decline the parts that
cannot be applied as written, and record a residual for each declined part separately. Two failure
shapes recur and neither is visible under a whole-remedy verdict:

- A finding is right on substance while its literal remedy is wrong. The remedy is declined; the
  claim is affirmed and the residual recorded. Declining the whole finding discards a real defect.
- A finding offers two remedies and the cheaper one does not converge. Diff size is not a
  selection criterion — grade each offered branch on whether it closes the defect class, and say
  which one was taken and why.

## What a completed adjudication looks like

Each premise carries a claim, a verdict from the vocabulary, and the evidence that settled it. The
finding as a whole carries no verdict of its own; its disposition follows from its premises. A
published entry whose premises cannot be read this way is published as unverified rather than
presented as settled — the honest state is the point, because an absence that renders identically to
a settled claim is the defect this recipe exists to prevent.
