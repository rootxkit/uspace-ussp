"use client";

// Sign-in: the kit's LoginForm on the BFF's /_bff/login. The password
// crosses the network once and the session token never reaches page
// script (M21). A refusal shows the API's detail; a lockout counts its
// Retry-After down. Signed in, the overview is the first page.
import Link from "next/link";
import { useRouter } from "next/navigation";
import { BFF_LOGIN_PATH, LoginForm } from "@rootxkit/uspace-ui/auth/client";
import { PageHeader, Panel } from "@rootxkit/uspace-ui/ui";
import { useAppT } from "@/i18n/t";

export function LoginPage() {
  const t = useAppT();
  const router = useRouter();
  return (
    <section aria-labelledby="login-heading" className="flex flex-col gap-6">
      <PageHeader title={<span id="login-heading">{t("portal.login.heading")}</span>} description={t("portal.login.note")} />
      <Panel>
        <LoginForm
          action={BFF_LOGIN_PATH}
          onSuccess={() => {
            router.push("/");
            router.refresh();
          }}
        />
      </Panel>
      <p className="m-0 text-center text-sm text-muted-foreground">
        {t("portal.login.no_account")}{" "}
        <Link href="/register" className="font-semibold text-foreground underline">
          {t("portal.nav.register")}
        </Link>
      </p>
    </section>
  );
}
