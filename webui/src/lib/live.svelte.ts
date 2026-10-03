import { api } from "./api";
import type { VerdictState } from "./types";

// The daemon's state, polled every two seconds for every page.
export const live = $state<{ state: VerdictState | null; error: string | null }>({ state: null, error: null });

export async function refresh() {
  try {
    live.state = await api.verdict();
    live.error = null;
  } catch (e) {
    live.error = (e as Error).message;
  }
}

refresh();
setInterval(refresh, 2000);
