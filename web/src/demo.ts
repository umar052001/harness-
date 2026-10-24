/** Self-contained example using fictional profile data. */
export const PORTFOLIO_PROMPT = `Build a responsive portfolio using the fictional profile below.
Write index.html, style.css, and a short README.md in the current directory.

Profile:
- Name: Alex Morgan
- Role: Software Engineer
- Focus: Backend services, developer tools, and accessible web applications.
- Contact: alex@example.com

Selected projects:
- Field Notes: An offline-first notes app with full-text search. TypeScript, SQLite.
- Queue Monitor: A dashboard for background jobs and retries. Go, PostgreSQL.
- Open Atlas: An accessible directory of public datasets. Python, HTML, CSS.

Experience:
- Northstar Studio: Software Engineer, 2023–present. Builds APIs and developer tools.
- Independent Developer: 2021–2023. Built websites for local organizations.

Use a black, white, and gray palette, subtle glass effects, readable typography,
and generous spacing. Include a hero, projects, experience, and contact section.
Make the layout work on phones and respect reduced-motion preferences.

Use plain HTML and CSS. No network requests, frameworks, package installs, or
external assets. Do not invent project URLs. The page must open directly from disk.
Verify that the files exist, then reply with a short summary.`
