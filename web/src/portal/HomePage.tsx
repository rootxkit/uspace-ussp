"use client";

// The portal's start page: what the portal does and where to go. Signed
// in, the operator's record (an operator_admin reads it) with its state.
import Link from "next/link";
import { useEffect, useState } from "react";
import { useSession } from "@rootxkit/uspace-ui/auth/client";
import { ProblemNotice } from "@/components/ProblemNotice";
import { useAppT } from "@/i18n/t";
import { useApi, type Schemas } from "@/lib/api";
import { Fact, PageHeading, Utc } from "./common";
import { useMe } from "./useMe";

function OperatorCard({ operatorId }: { operatorId: string }) {
  const t = useAppT();
  const api = useApi();
  const [op, setOp] = useState<Schemas["Operator"] | null>(null);
  const [error, setError] = useState<unknown>(null);
  useEffect(() => {
    let live = true;
    api
      .GET("/v1/accounts/operators/{operator_id}", { params: { path: { operator_id: operatorId } } })
      .then(({ data }) => live && setOp(data ?? null))
      .catch((e: unknown) => live && setError(e));
    return () => {
      live = false;
    };
  }, [api, operatorId]);
  if (error !== null) return <ProblemNotice error={error} />;
  if (op === null) return <p role="status">{t("portal.loading")}</p>;
  return (
    <section aria-labelledby="operator-heading" data-testid="operator" data-status={op.status} className="rounded border border-[var(--us-border)] p-3">
      <h2 id="operator-heading" className="m-0 text-base font-semibold">
        {op.display_name}
      </h2>
      <dl className="m-0 mt-2 grid gap-2 sm:grid-cols-3">
        <Fact term={t("portal.register.registration_number")}>{op.registration_number}</Fact>
        <Fact term={t("portal.operator.status")}>{t(`portal.operator.status.${op.status}`)}</Fact>
        <Fact term={t("portal.operator.validation_status")}>{op.validation_status ?? t("portal.operator.validation_none")}</Fact>
        <Fact term={t("portal.operator.validated_at")}>
          <Utc iso={op.validated_at} />
        </Fact>
      </dl>
    </section>
  );
}

export function HomePage() {
  const t = useAppT();
  const { session } = useSession();
  const me = useMe();
  return (
    <section aria-labelledby="home-heading" className="flex max-w-3xl flex-col gap-3">
      <PageHeading id="home-heading">{t("portal.home.heading")}</PageHeading>
      <p className="m-0">{t("portal.home.intro")}</p>
      <p className="m-0 text-sm text-[var(--us-text-muted)]">{t("portal.home.decides")}</p>
      {session === null ? (
        <p className="m-0 flex gap-4">
          <Link href="/login" className="underline">
            {t("portal.nav.login")}
          </Link>
          <Link href="/register" className="underline">
            {t("portal.nav.register")}
          </Link>
        </p>
      ) : me.kind === "failed" ? (
        <ProblemNotice error={me.error} />
      ) : me.kind === "loaded" && me.me.operator_id !== undefined && me.me.roles.includes("operator_admin") ? (
        <OperatorCard operatorId={me.me.operator_id} />
      ) : null}
    </section>
  );
}
