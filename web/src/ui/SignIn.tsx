import { Show, createSignal } from "solid-js";

export function SignIn(props: { error: string; onCode: (code: string) => void }) {
  const [code, setCode] = createSignal("");
  return (
    <div class="grid h-full place-items-center p-6">
      <form
        class="w-full max-w-sm space-y-4"
        onSubmit={(e) => {
          e.preventDefault();
          if (code().trim()) props.onCode(code().trim());
        }}
      >
        <h1 class="text-lg font-bold text-accent">Stavlos</h1>
        <p class="text-dim">
          This browser is not signed in. In the Stavlos terminal run <span class="text-text">/web open</span>, or paste the code from its link here.
        </p>
        <input
          class="w-full rounded border border-line bg-panel px-3 py-2 uppercase tracking-widest outline-none focus:border-accent"
          placeholder="one-time code"
          autocomplete="off"
          autocapitalize="characters"
          spellcheck={false}
          value={code()}
          onInput={(e) => setCode(e.currentTarget.value)}
        />
        <button class="w-full rounded bg-accent px-3 py-2 font-bold text-ink" type="submit">
          Sign in
        </button>
        <Show when={props.error}>
          <p class="text-err">{props.error}</p>
        </Show>
      </form>
    </div>
  );
}
