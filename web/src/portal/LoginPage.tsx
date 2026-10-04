"use client";

// Sign-in: the kit's LoginForm on the BFF's /_bff/login. The password
// crosses the network once and the session token never reaches page
// script (M21). A refusal shows the API's detail; a lockout counts its
// Retry-After down.
import Link from "next/link";
import { useRouter } from "next/navigation";
import { BFF_LOGIN_PATH, LoginForm } from "@rootxkit/uspace-ui/auth/client";
import { useAppT } from "@/i18n/t";
import { PageHeading } from "./common";

export function LoginPage() {
  const t = useAppT();
  const router = useRouter();
  return (
    <section aria-labelledby="login-heading" className="flex max-w-md flex-col gap-3">
      <PageHeading id="login-heading">{t("portal.login.heading")}</PageHeading>
      <p className="m-0 text-sm text-[var(--us-text-muted)]">{t("portal.login.note")}</p>
      <LoginForm
        action={BFF_LOGIN_PATH}
        onSuccess={() => {
          router.push("/intents");
          router.refresh();
        }}
      />
      <p className="m-0 text-sm">
        {t("portal.login.no_account")}{" "}
        <Link href="/register" className="underline">
          {t("portal.nav.register")}
        </Link>
      </p>
    </section>
  );
}
