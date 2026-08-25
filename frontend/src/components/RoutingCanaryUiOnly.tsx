import { useState } from 'react';

/* UI-only routing canary. A single accessible button that toggles from a
 * "ready" state to a "confirmed" state on activation, updating both its visible
 * text and accessible name. It has no backend dependency, so a passing manual or
 * automated check confirms that routing, rendering, and client-side state
 * updates are wired end-to-end. */

const READY_LABEL = 'UI routing canary ready';
const CONFIRMED_LABEL = 'UI routing canary confirmed';

export default function RoutingCanaryUiOnly() {
  const [confirmed, setConfirmed] = useState(false);
  const label = confirmed ? CONFIRMED_LABEL : READY_LABEL;

  return (
    <button type="button" aria-label={label} onClick={() => setConfirmed(true)}>
      {label}
    </button>
  );
}
