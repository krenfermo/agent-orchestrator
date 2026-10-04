WORKFLOW 2 — MEDUSA ADMINISTRATIVE ROC BACKUP / RESTORE MANAGEMENT (SERVER-DEVELOPMENT MODE)

Objective
Implement and validate a production-grade gallery backup and restore/import experience inside MEDUSA Configuration so an administrator does not need to know Linux filesystem paths, edit .env, SSH into the server, or manually copy roc.t files for normal backup operations.

This is a real autonomous AO implementation workflow running against the user's server-based development environment.

IMPORTANT ENVIRONMENT CONTEXT

The host at 192.168.10.163 is called "production" operationally, but in practice it is currently the user's private development server:
- only the user is using it
- there are no external production users depending on it yet
- this environment is explicitly authorized for active development, testing, rebuilds, restarts, and controlled data changes

AO may therefore use the server as the primary integration/test environment, not merely for read-only inspection.

SSH access:
ssh -i ~/.ssh/medusa_dhekav2 udheka@192.168.10.163

Backend server path:
cd /opt/medusa-node

Frontend server path:
cd /opt/medusa

Repository model

MEDUSA contains independent Git repositories.

Frontend/root MEDUSA repository:
- server path: /opt/medusa
- base branch: main

Backend repository:
- server path: /opt/medusa-node
- independent Git repository
- base branch: medusa_back_v2

Treat frontend and backend as completely independent repositories.
Never stage/commit backend files from the frontend repository or vice versa.

AO local/worktree model

AO should still use isolated AO-created worktrees/branches for implementation work where its multirepo workflow expects them.

However, unlike the earlier restricted version, this workflow IS authorized to deploy/test the resulting changes on the server-development environment after local verification.

Do not push or merge upstream unless explicitly requested.
Local commits in AO branches/worktrees are allowed and expected as part of the autonomous workflow.

SERVER DEVELOPMENT AUTHORIZATION

You are explicitly authorized to:

- SSH to 192.168.10.163
- inspect both /opt/medusa and /opt/medusa-node
- modify application source on the server when needed for integration testing
- pull/copy/sync the AO implementation into the server-development checkout in a controlled way
- build frontend/backend
- rebuild Docker images
- restart/recreate application containers
- run database migrations required by this feature
- create test backup records
- create test backup files
- validate backup/restore flows
- create temporary test galleries or use clearly disposable test fixtures
- run Playwright/browser E2E against the real server UI
- inspect DB, filesystems, mounts, logs and Docker runtime
- change application configuration needed by THIS feature
- update docker-compose/env wiring needed by THIS feature
- clean up only the temporary test fixtures created by this workflow

This authorization does NOT include careless/destructive actions.

NEVER:
- delete or truncate existing production/development roc.t files
- call remove/remove-all/defragment/compact on existing galleries
- wipe PostgreSQL
- delete Docker volumes
- docker system prune / docker volume prune
- delete /var/lib/medusa/roc-templates
- delete NFS source datasets
- overwrite an active gallery backup/restore target without an explicit, verified safe workflow
- modify unrelated users/data/configuration
- expose secrets in logs/reports
- push to remote
- merge to shared branches

If a restore test could alter an existing non-test gallery, use a temporary disposable gallery instead.

KNOWN SERVER PATHS TO VERIFY, NOT BLINDLY ASSUME

Backend:
- /opt/medusa-node

Frontend:
- /opt/medusa

Likely current configuration:
- ROC_GALLERY_ROOT=/app/templates
- ROC_TEMPLATES_HOST_PATH=/var/lib/medusa/roc-templates
- ROC_IMPORT_ROOT=/imports/roc
- ROC_IMPORT_HOST_PATH=/var/lib/medusa/imports
- STORAGE_ROOT=/storage/medusa

Use:
- docker compose config
- docker inspect
- actual .env files
to confirm runtime truth.

PRODUCT GOAL

From Configuración, an Administrator should be able to:

1. See all available ROC galleries.
2. Create a backup of a selected gallery with one click.
3. See existing backups.
4. See backup metadata:
   - gallery
   - source gallery id
   - internal ROC identifier
   - created_at
   - created_by
   - roc.t size
   - SHA-256
   - ROC active template count
   - removed template count
   - application version/build when available
5. Download a backup when appropriate.
6. Validate a backup.
7. Restore/import a backup through a safe wizard.
8. Never type /var/lib/... or /imports/roc paths in the normal UX.
9. Keep an audit trail for backup, validation, restore/import and failures.
10. Prevent accidental overwrite/destruction of a live gallery.

UX PRINCIPLE

Filesystem roots are infrastructure details.

Normal users should see:
- Galería
- Respaldar
- Respaldos
- Validar
- Descargar
- Restaurar / Importar

They should not need to know:
- /var/lib/medusa/...
- /imports/roc
- TEMPLATES
- TEMPLATE_* env variables

PHASE 0 — DISCOVERY ON REAL SERVER

Before implementation, inspect the real server.

SSH:
ssh -i ~/.ssh/medusa_dhekav2 udheka@192.168.10.163

Inspect backend:
cd /opt/medusa-node

Inspect frontend:
cd /opt/medusa

Confirm:
- repository status/branches
- docker-compose layout
- running containers
- real mounts
- Configuración UI implementation
- existing ROC import APIs/services
- existing audit APIs/services
- centralized RBAC
- stored_files/download helpers
- roc-admin inventory integration
- advisory/exclusive ROC locking
- migrations/schema conventions

Use the actual server implementation as authoritative evidence.

ARCHITECTURAL REQUIREMENTS

1. Reuse existing functionality.
Do not duplicate current ROC import/validation infrastructure.

2. Reuse where appropriate:
- ROC import validation
- roc-admin inventory
- gallery repository/services
- audit infrastructure
- stored_files/download infrastructure
- centralized authz.ts
- frontend permissions.ts
- existing Configuración UI patterns
- existing file streaming/storage primitives

3. Admin-only
All backup/restore administrative actions must be ADMIN only.
Do not create ad-hoc role sets.

4. Backup persistence
Choose a persistent server-backed storage design after inspecting actual mounts.

Preferred:
- managed ROC backup root on persistent host storage
- no operator-entered filesystem paths

A reasonable target may be something like:
- host: /var/lib/medusa/roc-backups
- container: /backups/roc

BUT do not blindly adopt this exact path if an existing persistent storage abstraction is better.

If adding env/mounts:
- add safe defaults
- update Docker Compose
- update install/deploy docs
- make runtime behavior deterministic
- keep raw paths out of normal frontend UX

5. Backup creation must be crash-safe and non-destructive.

Minimum:
- acquire appropriate read/exclusive consistency lock
- verify source exists and is a regular file
- copy/stream to temporary destination
- do not buffer huge roc.t files fully in memory
- fsync if appropriate
- SHA-256
- roc-admin inventory the backup copy
- atomically finalize
- persist DB metadata only after valid backup exists
- clean partial temp files on failure
- never mutate source roc.t

6. Backup artifact

At minimum:
- roc.t
- manifest.json

manifest should include:
- format/version
- original gallery id/name
- internal identity
- created_at
- created_by
- file size
- SHA-256
- active/removed counts
- MEDUSA version/build if available

Never include secrets.

7. Restore/import safety

Default must NOT overwrite a live gallery.

Preferred semantics:
- choose backup
- validate
- preview
- restore/import as a NEW gallery or into an explicitly safe empty target
- detect identity/path collisions
- final explicit admin confirmation
- execute
- verify resulting roc.t hash/inventory
- verify DB/runtime state

If existing code already supports a safe replacement flow, reuse it.
Otherwise do not invent a dangerous overwrite feature.

8. Never call:
- remove
- remove-all
- defragment
- compact

9. Preserve canonical gallery identity rules.
Do not reintroduce free-form legacy identifiers in UI.

10. Native addon compatibility
Internal ROC identifiers must use the current safe single-token service naming convention unless/until the vendor addon is fixed.
Do not migrate unrelated existing galleries in this workflow.

DATABASE

Decide whether a dedicated backup table is appropriate.

If yes, follow existing migration/schema conventions.

Likely fields:
- id
- gallery_id
- backup key/name
- backup directory or stored_file references
- manifest
- size
- sha256
- active_template_count
- removed_template_count
- status
- created_by
- created_at
- validated_at
- validation result
- metadata_json

Migrations must be idempotent/safe for the development server.

API

Design endpoints consistent with current backend conventions.

Capabilities likely include:
- list backups
- backup detail
- create backup
- validate
- download
- restore/import
- async operation status if necessary

Use streaming for large files.

FRONTEND

Implement inside the real frontend repo at:
/opt/medusa

Suggested Configuración experience:

Configuración
  Galerías ROC
    gallery list
    action: Respaldar

  Respaldos ROC
    Galería
    Fecha
    Templates
    Tamaño
    Estado
    Acciones:
      Ver detalles
      Validar
      Descargar
      Restaurar / Importar

Backup flow:
- select gallery
- show current inventory/size
- confirm
- progress
- success/failure

Restore/import wizard:
1. select backup
2. validate
3. show manifest/inventory
4. choose safe target
5. preview
6. confirm
7. execute
8. verify

Do not redesign unrelated screens.

AUDIT

Record:
- backup created
- validation succeeded/failed
- download when appropriate
- restore/import requested
- restore/import completed/failed

Include actor + gallery + backup identifiers.

TESTS

Backend:
- RBAC
- valid backup
- source missing
- non-regular/symlink/path traversal handling
- copy failure
- checksum correctness
- manifest correctness
- inventory validation
- temp cleanup
- retry/idempotency
- collision handling
- safe restore preview
- restore refusal for unsafe target
- no destructive ROC calls
- large-file streaming behavior

Frontend:
- admin sees controls
- analyst does not
- create backup
- progress/error
- backup list/detail
- validation
- restore preview/confirm
- routing/access guards

Cross-repo:
- verify API types/contracts
- ensure frontend matches backend payloads

REAL SERVER E2E VALIDATION

After implementation/tests in isolated AO worktrees:

Deploy the feature to the server-development environment in a controlled way.

Backend server:
- /opt/medusa-node

Frontend server:
- /opt/medusa

You may:
- update server checkouts from the AO implementation
- run migrations
- build/rebuild
- recreate affected containers/services
- run Playwright against the real UI

Use a DISPOSABLE TEST GALLERY for destructive restore/import validation.

Required E2E:

1. Login as admin.
2. Open Configuración.
3. Choose a disposable gallery with a valid roc.t.
4. Create backup from UI.
5. Confirm:
   - backup row exists
   - backup file exists
   - SHA-256 matches source at creation time
   - inventory matches source
   - manifest valid
6. Download backup through UI/API if implemented.
7. Validate backup.
8. Restore/import into a NEW disposable gallery.
9. Confirm:
   - restored roc.t exists
   - hash/inventory matches backup
   - DB gallery exists
   - thumbnails/data references are correct for the semantics implemented
   - gallery is operable according to current ROC limitations
10. Restart/recreate relevant containers.
11. Confirm backup metadata/artifacts persist.
12. Confirm restored test gallery persists.
13. Login as Analista.
14. Verify backup/restore controls are invisible and API returns 403.

Do not use an important existing gallery for restore testing.

CLEANUP

At the end:
- keep code/migrations
- remove/deactivate only disposable E2E test galleries/users created by this workflow when safe
- do NOT delete backup fixtures if they are needed as evidence; otherwise clearly mark/remove only workflow-created disposable artifacts
- leave server in healthy state

DOCUMENTATION

Update:
- admin guide
- backup flow
- restore/import flow
- persistent backup storage
- Docker/env requirements
- disaster recovery prerequisites

Document the minimum MEDUSA DR set:
- PostgreSQL
- ROC templates persistent storage
- stored_files persistent storage
- ROC backup storage
- any other proven required persistent state

FINAL VERIFICATION

Backend:
- typecheck
- build
- relevant tests

Frontend:
- typecheck
- build
- relevant tests

Reviewer:
- data safety
- locking/races
- path traversal
- symlink handling
- RBAC
- restore safety
- error cleanup
- persistence
- cross-repo contract

Fix all changes_requested findings.

FINAL REPORT

Return:

1. Architecture implemented.
2. Real server paths/mounts confirmed.
3. Files changed per repo.
4. DB migrations.
5. API contract.
6. Backup storage model.
7. Restore safety model.
8. RBAC.
9. Audit.
10. Tests.
11. Build/typecheck.
12. Real server E2E results.
13. Screenshots.
14. Backup SHA/inventory evidence.
15. Restore/import evidence.
16. Restart/recreate persistence result.
17. Any remaining risks.
18. Git commits in AO branches/worktrees.
19. Server git status for /opt/medusa and /opt/medusa-node.
20. Explicit confirmation:
   - no remote push
   - no remote merge
   - no unrelated production/development data modified
21. READY FOR HUMAN REVIEW verdict.

Execution constraints for this MEDUSA autonomous workflow:

- This is a real autonomous workflow using the user's server-based development environment.
- Work only inside AO-created isolated worktrees/branches for implementation.
- Do not modify the user's normal local checked-out branches directly.
- Root/frontend MEDUSA repository base: main.
- backend_node repository base: medusa_back_v2.
- Treat backend_node as an independent Git repository.
- Frontend server checkout: /opt/medusa.
- Backend server checkout: /opt/medusa-node.
- You may modify either repository, or both, only if the objective genuinely requires it.
- Preserve repository boundaries.
- Do not push.
- Do not merge upstream.
- Server deployment/testing IS authorized for this workflow.
- You may modify/rebuild/restart the server-development environment as required for this feature.
- You may run required DB migrations.
- You may create/modify/delete only disposable test fixtures created by this workflow.
- Do not modify or destroy unrelated existing biometric galleries or ROC data.
- Run all relevant tests/checks for every repository you change.
- If blocked by a genuine human decision or an operation that could destroy unrelated data, stop and surface the decision through AO instead of guessing.
- Otherwise continue autonomously through plan, implementation, review, fixes, server deployment, E2E verification, cleanup, and final integration report.
- Leave both the AO worktrees and the server-development environment in a reviewable, healthy state.
