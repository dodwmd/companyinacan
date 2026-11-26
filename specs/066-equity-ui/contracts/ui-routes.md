# can-equity web UI routes

The route table of the web UI of [specs/066](../spec.md). `core/equity/ui_quality_test.go` fails when a
route is served and not listed, or listed and not served. "Gate" is the Service method that decides; each
asks the policy layer (`core/equity/policy.go`) again with the facts of the record. "Step-up" means the
session must be confirmed again for that post.

| Route | Session | Gate | Step-up |
|---|---|---|---|
| `GET /login` | no | none (sign-in form) | no |
| `POST /login` | no | the API's Authenticator chain; never the service account | no |
| `GET /login/ticket` | no | can-core launch ticket (does not spend it) | no |
| `POST /login/ticket` | no | can-core launch ticket | no |
| `POST /logout` | yes | own session | no |
| `GET /{$}` | yes | MyPositions, SummaryCounts, each only where the policy allows | no |
| `GET /me` | yes | MyPositions, MyConfirmedExercises | no |
| `GET /me/documents` | yes | MyPositions | no |
| `GET /me/grants/{id}` | yes | MyPosition | no |
| `GET /me/grants/{id}/statement` | yes | MyPosition | no |
| `POST /me/grants/{id}/exercise/review` | yes | PreviewMyExercise (records nothing) | no |
| `POST /me/grants/{id}/exercise` | yes | RequestMyExercise | no |
| `POST /me/exercises/{id}/cancel` | yes | CancelMyExercise | no |
| `GET /company` | yes | ListEntities | no |
| `GET /company/entities/{id}` | yes | GetEntity, ListShareClasses, ListPlans, Pool | no |
| `GET /company/entities/{id}/captable` | yes | GetCapTable | no |
| `GET /company/entities/{id}/exports` | yes | ExportForm | no |
| `POST /company/entities/{id}/exports` | yes | ExportCSV | no |
| `GET /company/entities/{id}/stakeholders` | yes | ListStakeholders, HolderNames | no |
| `POST /company/entities/{id}/stakeholders` | yes | CreateStakeholder | no |
| `GET /company/stakeholders/{id}` | yes | GetStakeholder, GetIdentity, ListGrants | no |
| `POST /company/stakeholders/{id}/verify` | yes | VerifyPerson | yes |
| `POST /company/stakeholders/{id}/attest` | yes | AttestExternal | yes |
| `GET /company/identity` | yes | IdentityReview | no |
| `GET /company/entities/{id}/grants` | yes | ListGrants | no |
| `GET /company/entities/{id}/grants/new` | yes | DraftForm | no |
| `POST /company/entities/{id}/grants` | yes | CreateGrant | no |
| `GET /company/grants/{id}` | yes | GetGrant, GetEntity, Vesting, ListExercises | no |
| `POST /company/grants/{id}/submit` | yes | SubmitGrant | no |
| `POST /company/grants/{id}/delete` | yes | DeleteGrant | yes |
| `POST /company/grants/{id}/approve` | yes | ApproveGrant | yes |
| `POST /company/grants/{id}/reject` | yes | RejectGrant | no |
| `POST /company/grants/{id}/activate` | yes | ActivateGrant | yes |
| `GET /company/entities/{id}/exercises` | yes | ListExercises | no |
| `GET /company/exercises/{id}` | yes | GetExercise, GetEntity | no |
| `POST /company/exercises/{id}/approve` | yes | ApproveExercise | no |
| `POST /company/exercises/{id}/reject` | yes | RejectExercise | no |
| `POST /company/exercises/{id}/cancel` | yes | CancelExercise | no |
| `POST /company/exercises/{id}/payment` | yes | RecordPayment | yes |
| `POST /company/exercises/{id}/issue` | yes | IssueShares | yes |
| `GET /static/{file}` | no | embedded stylesheet and icon | no |
| `GET /manifest.webmanifest` | no | embedded manifest | no |
