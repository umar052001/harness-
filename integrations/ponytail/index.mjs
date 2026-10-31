// A directory entrypoint is required by the installed OpenCode V2 loader.
// Execute the pinned upstream plugin unchanged; do not duplicate its rules.
export { default } from '../../.tools/node_modules/@dietrichgebert/ponytail/.opencode/plugins/ponytail.mjs'
