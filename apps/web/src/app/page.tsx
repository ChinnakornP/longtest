import { Button } from '@/components/ui/button';

export default function HomePage() {
  return (
    <main className="mx-auto flex min-h-screen max-w-2xl flex-col justify-center gap-6 px-6">
      <div className="space-y-2">
        <h1 className="text-3xl font-semibold tracking-tight">AI QA Agent</h1>
        <p className="text-muted-foreground text-sm">
          Test your web application automatically.
        </p>
      </div>

      <div className="rounded-lg border p-6 text-sm">
        <p className="text-muted-foreground">
          Stage-1 placeholder. The project form, runtime picker, live run view and artifact
          viewer are delivered by T8.
        </p>
      </div>

      <Button disabled>Start Testing</Button>
    </main>
  );
}
