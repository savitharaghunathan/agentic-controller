---
name: execute
description: >
  Reads PLAN.md and executes each migration step sequentially for Java EE to
  Quarkus 3 migrations. Applies transformations file by file using the bundled
  reference patterns. Handles pom.xml, application.properties, EJB-to-CDI, MDB
  conversions, JNDI removal, and config file cleanup. Use after the plan stage
  has produced PLAN.md.
---

# Execute Stage (Java)

Executes the approved migration plan from `PLAN.md`, one file at a time.
Works autonomously — processes all items in sequence without waiting.

## References

- [references/javaee-quarkus.md](references/javaee-quarkus.md) — full
  transformation pattern catalog with import maps, annotation replacements,
  and before/after examples for every pattern type
- [references/execute-recipe.yaml](references/execute-recipe.yaml) — structured
  recipe with parameters, response schema, and per-item execution instructions

## Startup Sequence

1. Read `PLAN.md` from the repo root — read it ONCE
2. Read [references/javaee-quarkus.md](references/javaee-quarkus.md) for the
   full transformation pattern catalog
3. Begin executing steps in order, starting with Step 1

Do NOT read any source files before starting Step 1.

---

## Per-File Execution Loop

For each step in PLAN.md, follow this exact sequence:

```
1. Read the target file
2. Apply transformations per the step's instructions and reference patterns
3. Write the modified file
4. Move to the next step immediately
```

### Guardrails

- You MUST attempt every item in PLAN.md in order. Do not skip items.
- After completing each item, note it mentally before moving to the next.
- Do not re-read PLAN.md after every item — read it once, work through the list.
- If you cannot complete an item, note the reason and move to the next.
  Do not get stuck on one item.

---

## Handling Special File Types

### pom.xml
- Change `<packaging>war</packaging>` to `<packaging>jar</packaging>`
- Remove `javaee-api` dependency and `maven-war-plugin`
- Add Quarkus BOM in `<dependencyManagement>`
- Add `quarkus-maven-plugin` in `<build><plugins>`
- Add only the extensions the project actually needs (check what is used in source)
- Do NOT add extensions speculatively

### application.properties (CREATE NEW if missing)
- Replaces `persistence.xml` datasource config
- Replaces `web.xml` HTTP config
- Add AMQP messaging config only if MDB files exist in the project
- Use `%dev.*` profile for local dev settings

### Non-MDB Service files (@Stateless / @Stateful)
- Replace `javax.*` to `jakarta.*` imports
- Replace `@Stateless` / `@Stateful` to `@ApplicationScoped`
- Replace `@EJB` to `@Inject`
- Remove `@Local`, `@Remote`, JNDI lookup code
- Remove Remote interface files entirely

### MDB files (@MessageDriven)
- Replace entire class structure — see pattern in references/javaee-quarkus.md
- Use `@Incoming` channel name derived from the queue/topic name in the original config
- Add matching `mp.messaging.incoming.*` to application.properties

### DELETE items
- Delete the file
- If file does not exist, note as already done and move on

---

## Import Transformations

Apply to every file — simple find-and-replace:

```
javax.ejb.*              → REMOVE (handle via annotation changes below)
javax.inject.*           → jakarta.inject.*
javax.enterprise.*       → jakarta.enterprise.*
javax.persistence.*      → jakarta.persistence.*
javax.ws.rs.*            → jakarta.ws.rs.*
javax.transaction.*      → jakarta.transaction.*
javax.json.*             → jakarta.json.*
javax.xml.bind.*         → jakarta.xml.bind.*
javax.validation.*       → jakarta.validation.*
javax.annotation.*       → jakarta.annotation.*
javax.jms.*              → REMOVE (replace with SmallRye Reactive Messaging)
weblogic.*               → REMOVE (no replacement)
org.jboss.ejb.*          → REMOVE
org.wildfly.*            → REMOVE
```

## Annotation Transformations

```
@Stateless               → @ApplicationScoped
@Stateful                → @ApplicationScoped
@Singleton (EJB)         → @ApplicationScoped  (use jakarta.enterprise, not javax.ejb)
@EJB                     → @Inject
@Local                   → REMOVE
@Remote                  → REMOVE
@TransactionAttribute    → @Transactional  (jakarta.transaction)
```

---

## Completion

After executing all steps, append your result to `.konveyor/result.json`:

Read the existing file (it should have the plan stage entry), parse the
JSON array, append your entry, and write it back.

Your entry:

```json
{"stage": "execute", "status": "succeeded"}
```

Or on failure:

```json
{"stage": "execute", "status": "failed", "reason": "<what went wrong>"}
```

## Important

- Work through ALL items — completeness matters more than perfection
- Follow the reference file for complex patterns (MDB, JNDI)
- Do NOT run builds or tests — that is the verify stage's job
- Do NOT modify PLAN.md
