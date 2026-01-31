## Project Summary
Orchids OS is a high-fidelity digital workspace and navigation hub built for speed, precision, and aesthetic dominance. It features an integrated autonomous navigation grid, a Phaser-based gaming sector, real-time system state synchronization, and a suite of AI-powered tools for deep analysis and synthesis.

## Tech Stack
- **Frontend**: Next.js 15 (App Router), React 19, Tailwind CSS 4
- **UI/UX**: Radix UI, Framer Motion, Motion 12+, Lucide Icons
- **Typography**: Space Grotesk (Primary), Geist Sans/Mono
- **Auth**: Better Auth (src/lib/auth.ts)
- **Database**: Drizzle ORM, postgres.js, Supabase
- **Payments**: Stripe (Stripe SDK + React Stripe)
- **Game Engine**: Phaser 3.90 (src/components/mario)
- **State Management**: NumberFlow for fluid numeric transitions
- **AI Stack**: AI SDK (Vercel), OpenAI (gpt-4o-mini, whisper-1, tts-1), pdf-parse

## Architecture
- `src/app`: Next.js App Router (pages, layouts, API routes)
- `src/app/nav`: Universal Navigation Grid implementation
- `src/app/ai`: Neural Intelligence Suite (Chat, PDF Analysis, Image Gen, Voice, Translate)
- `src/components`: Feature-specific components
- `src/components/ui`: Low-level reusable UI components (Shadcn UI)
- `src/hooks`: Custom React hooks (e.g., `useCounter`)
- `src/lib`: Core utilities, database configuration, auth setup, AI clients

## Commands
### Development & Build
- `npm run dev`: Start development server with Turbopack
- `npm run build`: Build production application
- `npm run start`: Start production server
- `npm run lint`: Run ESLint checks
- `npm run typecheck`: Run TypeScript compiler check

### Database
- `npx drizzle-kit generate`: Generate migrations
- `npx drizzle-kit push`: Push schema changes to database
- `npx drizzle-kit studio`: Open database UI

## Code Style Guidelines
### 1. Aesthetic Philosophy (Orchids OS)
- **Theme**: Extreme dark mode (#020202 base) with deep midnight accents and neon primary highlights.
- **Glassmorphism**: Use high-translucency backgrounds (`bg-white/[0.02]`), sharp borders (`border-white/5`), and multi-layered backdrop blurs (`backdrop-blur-3xl`).
- **Typography**: Use `Space Grotesk` with `font-black` for high-impact headings and `italic` for metadata/descriptions. Use tracking-tighter for a premium look.
- **Micro-interactions**: Every interactive element should have a hover response (scale, rotate, y-translation, or inner glow).

### 2. Components & Layout
- **Bento Grids**: Prefer large, rounded-corner cards (`rounded-[2.5rem]` or `rounded-[3rem]`) for content organization.
- **Spacing**: Use generous vertical spacing (`space-y-32`) to create a sense of scale and focus.
- **Animations**: Use `framer-motion` for staggered entrance animations and spring-based physics for hover states.

## Common Patterns
- **Universal Search**: Implementation in `/nav` using real-time filtering and motion-based feedback.
- **Autonomous Navigation**: Hierarchical organization of link nodes into sectors (categories) with full CRUD capabilities.
- **System Synchronization**: Use `global_counter` and `VisitCounter` for real-time ecosystem stats.
- **Mario Integration**: Gaming sector with hat wardrobe system reflecting in the Phaser game engine.
- **Neural Uplink**: Consistent AI interaction patterns using `useChat` and streaming responses.

## User Preferences
- **No Comments**: Do not add comments to code unless explicitly requested.
- **Visual Edits**: Next.js project configured for Orchids Visual Edits.
- **Linting/Typechecking**: Always run `npm run lint` and `npm run typecheck` after changes.
