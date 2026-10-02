/**
 * The prompt behind the "portfolio demo" button in settings.
 *
 * It carries the profile data inline on purpose. The first version of this
 * prompt told the agent to fetch janjua.dev itself, and the demo spent its whole
 * ten-minute budget on retrieval: the site is a client-rendered React app, so the
 * HTML the agent got back was an empty `#root`, and it fell back to grepping a
 * web.archive.org snapshot byte by byte. It never reached the part that mattered -
 * writing the page.
 *
 * So the content is here, and the prompt asks for nothing from the network. What
 * is left is the part worth watching: an agent turning a spec into files.
 */
export const PORTFOLIO_PROMPT = `Build a polished single-page portfolio site and write it into the current directory.

Do not fetch anything from the network. All the content you need is below - use it
verbatim and do not invent projects, dates or links.

## Profile

Name: Muhammad Umar Nawaz Janjua
Role: Software Engineer - Backend, AI & Systems
Location: Riyadh, Saudi Arabia
GitHub: https://github.com/umar052001
LinkedIn: https://linkedin.com/in/muhammad-umar-nawaz-janjua-99a702233
Email: umar.111nawaz@gmail.com

## Selected projects

1. structmd - Vision-Language to Structured Markdown. Python, Ollama, Vision-Language Models, PyMuPDF, PyPI. https://github.com/umar052001/structmd
2. Blitz Parse - High-Performance PDF Extraction in Rust. Rust, Python bindings, PyPI. https://github.com/umar052001/blitzparse
3. AI Research Agent CLI - Autonomous Research Engine. Python, AsyncIO, Ollama, Web Crawling. https://github.com/umar052001/AI-research-agent
4. Dev Music AI - Music Search, Player and Downloader. Go, React, yt-dlp, SQLite, Pluggable LLMs. https://github.com/umar052001/dev-music-ai
5. Invoice Watcher - Automated Invoice Processing. Electron.js, React, File Watcher, Google Sheets API.
6. HarmonySketch - AI Music Theory and Composition. Web Audio API, React, AI Composition.
7. Email Verifier Engine - High-Scale Verification. Python, AsyncIO, FastAPI, React, GCP, Redis.
8. Mandumah RAG Agent - Arabic Academic Research. LangGraph, Vertex AI, LlamaIndex, AraBERT.
9. B2B Leads Platform - Imagini, KSA. FastAPI, React, PostgreSQL, Streaming.
10. Natural Language DB Interface. Python, FastAPI, React, Gemini, SQLAlchemy.

## Experience

- Apex AI Arabia - Software Engineer, Jul 2026 to Present, Saudi Arabia (Remote). Go, Rust, gRPC, PostgreSQL, RAG pipelines, multi-agent systems.
- Modern Intelligent Solutions - Software Engineer, Jun 2024 to Jan 2026, Riyadh, Saudi Arabia. FastAPI, PostgreSQL, async pipelines, LLM integration.
- Voilabs - Software Engineer, Nov 2023 to May 2024, Paris, France (Remote).
- Self-Employed - Open Source Developer and Hackathon Winner, Jan 2022 to Present, Remote.

## What to build

Write exactly three files:

- index.html - semantic markup: a hero, a skills strip, a grid of the ten
  projects, an experience timeline, and a contact footer with the links above.
- style.css - all styling. Dark theme, one accent colour, responsive down to
  phone width. Cards or timeline entries that read well in a grid.
- README.md - three or four lines on what you built.

Constraints, all of them deliberate:

- No frameworks, no npm, no build step, no CDN. Plain HTML and CSS only.
  index.html must open correctly by double-clicking it.
- No JavaScript. It is a static page.
- Keep style.css under about 500 lines and index.html under about 400.
- Link every project to its repository when one is listed; otherwise show the
  project title with no dead link.
- Get it working and looking good rather than adding features.

When the three files are written, stop. Reply with at most five bullet points on
what you built - do not paste file contents back, and do not re-read the files to
summarise them.`