// SPDX-License-Identifier: Apache-2.0

package v1alpha1

// AnnotationMigrating marks a Room or Task that maku-broker migrate-group has copied but whose status
// it has not written yet. The Room controller and the factory leave such an object alone until the
// annotation is gone: with no status it would read as new (the maku rebrand's group move).
const AnnotationMigrating = "maku.ogenki.io/migrating"

// AnnotationMigratedFrom is the UID of the object a migrate-group copy was made from. A re-run
// finishes a copy that carries its original's UID and refuses a same-named object that does not.
const AnnotationMigratedFrom = "maku.ogenki.io/migrated-from"
