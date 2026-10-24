import { configDefaults, defineConfig } from "vitest/config";

// A hidden directory holds a tool's state, never this repository's tests. A
// Claude Code worktree under .claude/worktrees is a whole checkout, and without
// this its copies of the tests run too, failing on a client it never built.
export default defineConfig({
  test: {
    exclude: [...configDefaults.exclude, "**/.*/**"],
  },
});
