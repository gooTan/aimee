import { mixedCanaryMessage } from '../canaries/mixedContract';

/* Canary surfacing the mixed routing state from mixedContract in a status
 * region so the mixed routing integration is visibly rendered and regressable. */

export default function RoutingCanaryMixed() {
  return (
    <div role="status" aria-label="Mixed routing canary">
      {mixedCanaryMessage}
    </div>
  );
}
