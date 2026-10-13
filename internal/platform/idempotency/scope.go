// Responsibilities: share the immutable authorized command scope across build
// modes. Dependencies: none; HTTP adapters and the pgx DAO use this DTO.
// Invariant: actor and tenant fields come from validated server context.
package idempotency

type Scope struct{ TenantID, ActorKind, ActorID, Operation, Key string }
