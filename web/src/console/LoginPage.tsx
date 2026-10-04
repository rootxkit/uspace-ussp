"use client";

// The console's sign-in (brief WP-18): the kit's LoginForm on the
// console's BFF route (realm console). A staff admin's password step is
// answered with a challenge and the form asks for the authenticator's
// code (the kit's second step); the password crosses the network once
// and the session token never reaches page script (M21).
import { useRouter } from "next/navigation";
import { LoginForm } from "@rootxkit/uspace-ui/auth/client";
import { useAppT } from "@/i18n/t";
import { CONSOLE_BFF } from "@/lib/bff/paths";
import { PageHeading } from "@/portal/common";

export function ConsoleLoginPage() {
  const t = useAppT();
  const router = useRouter();
  return (
    <section aria-labelledby="console-login-heading" className="flex max-w-md flex-col gap-3">
      <PageHeading id="console-login-heading">{t("console.login.heading")}</PageHeading>
      <p className="m-0 text-sm text-[var(--us-text-muted)]">{t("console.login.note")}</p>
      <LoginForm
        action={CONSOLE_BFF.login}
        onSuccess={() => {
          router.push("/console");
          router.refresh();
        }}
      />
    </section>
  );
}
