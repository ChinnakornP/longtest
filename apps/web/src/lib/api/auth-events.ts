export type AuthEvent = 'unauthenticated' | 'forbidden';
type Listener = (event: AuthEvent) => void;

const listeners = new Set<Listener>();

/**
 * Lets apiFetch (a plain module, not a component) tell the app "redirect
 * to login" / "redirect to no-access" without importing next/navigation.
 * A client component (see providers.tsx) subscribes and does the routing.
 */
export const authEvents = {
  subscribe(listener: Listener): () => void {
    listeners.add(listener);
    return () => listeners.delete(listener);
  },
  emit(event: AuthEvent): void {
    for (const listener of listeners) listener(event);
  },
};
