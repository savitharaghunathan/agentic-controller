---
name: verify-java
description: >
  Runs mvn clean compile, parses compiler errors, applies conservative fixes,
  and iterates until the build passes or max iterations are reached. Handles
  common post-migration errors: missing javax imports, missing Quarkus extensions,
  dangling references to deleted classes, and missing config properties. Use after
  the execute stage has finished migrating source files.
---

# Verify Stage (Java)

Verifies the migrated codebase compiles and tests pass. Can attempt
targeted fixes for compilation errors, up to a configurable iteration limit.

## References

- [references/verify-recipe.yaml](references/verify-recipe.yaml) — structured
  recipe with parameters, response schema, and verification+auto-fix instructions
- [references/fix-recipe.yaml](references/fix-recipe.yaml) — structured recipe
  for fixing a single compilation error (conservative, minimal edits)

---

## Phase 1 — Initial Build

Run the full compile:

```bash
mvn clean compile 2>&1 | tail -50
```

If the build succeeds (exit code 0), skip to Phase 4 (Run Tests).

If the build fails, extract the errors:

```bash
mvn clean compile 2>&1 | grep -E "ERROR|error:" | head -10
```

---

## Phase 2 — Fix Errors

For each compiler error:

1. Read the error message to identify the file and issue
2. Read the source file
3. Apply a minimal, conservative fix
4. Do NOT change code that is not related to the error

### Common Errors and Fixes

| Error | Fix |
|---|---|
| `package javax.* does not exist` | Find remaining `javax.*` import, replace with `jakarta.*` |
| `cannot find symbol @Incoming` | Add `quarkus-smallrye-reactive-messaging-amqp` to pom.xml |
| `cannot find symbol @ApplicationScoped` | Add `quarkus-arc` to pom.xml |
| `cannot find symbol` for removed class | Check if a deleted interface/class is still referenced; update the reference |
| `cannot find symbol Emitter` | Add `@Channel` import from `org.eclipse.microprofile.reactive.messaging` |
| `ClassNotFoundException weblogic` | Delete the `src/main/java/**/weblogic/` directory |
| `EntityManager cannot be injected` | Verify `@PersistenceContext` annotation is present |
| Missing `application.properties` keys | Add the required config property |

### Fix Rules

- Fix ONLY compiler errors, not warnings
- Minimal changes only — do not refactor working code
- Only touch the file reported in the error (or build manifest if it is a missing dependency)
- If a fix requires a new Quarkus extension, add it to pom.xml

---

## Phase 3 — Re-verify

After fixing errors, run the build again:

```bash
mvn clean compile 2>&1 | tail -50
```

Repeat Phases 2-3 up to the number of iterations specified by
`KONVEYOR_PARAM_MAX_FIX_ITERATIONS` (read from environment, default 3).

If the build still fails after max iterations, report failure with
the remaining errors.

---

## Phase 4 — Run Tests (if build passes)

```bash
mvn test 2>&1 | tail -80
```

Report test results (passed/failed/total counts) but do NOT attempt
to fix failing tests. Test failures are expected after a migration and
are documented in the result, not fixed here.

---

## Phase 5 — Write Result

Append your result to `.konveyor/result.json`:

Read the existing file (it should have plan and execute entries),
parse the JSON array, append your entry, and write it back.

Your entry on success:

```json
{"stage": "verify", "status": "succeeded"}
```

On failure:

```json
{"stage": "verify", "status": "failed", "reason": "mvn compile failed after N fix iterations: <remaining errors>"}
```

---

## Important

- Fixes must be minimal and conservative — do not rewrite working code
- Only fix compiler errors, not warnings
- Do NOT modify PLAN.md or files unrelated to the error
- Read `KONVEYOR_PARAM_MAX_FIX_ITERATIONS` from environment for iteration cap
- Track how many fix iterations you have attempted
- Report remaining errors in the result reason if build still fails
