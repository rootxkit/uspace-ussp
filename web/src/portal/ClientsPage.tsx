"use client";

// The operator's machine clients (brief WP-17): the list with scopes,
// status and bound serials (GET .../clients, any role), and for an
// operator_admin: create a client with its scopes (the secret is shown
// once, here, and never again), rotate a secret (the previous one works
// until the time the API answers), bind a serial with its class label
// and unbind one. Every refusal is the API's problem as it came.
import { useCallback, useEffect, useState } from "react";
import { z } from "zod";
import { RequireRole } from "@rootxkit/uspace-ui/auth/client";
import { CheckboxField, EnumField, Form, TextField } from "@rootxkit/uspace-ui/form";
import { Button, Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@rootxkit/uspace-ui/ui";
import { ProblemNotice } from "@/components/ProblemNotice";
import { useAppT } from "@/i18n/t";
import { useApi, type Schemas } from "@/lib/api";
import { Fact, PageHeading, RequireSession, Utc } from "./common";
import { useMe } from "./useMe";

const SCOPES = ["ussp.intents", "ussp.telemetry", "ussp.traffic", "ussp.geo"] as const;
const CLASS_LABELS = ["C0", "C1", "C2", "C3", "C4", "C5", "C6"] as const;

// A field name with a dot is a nested path to react-hook-form: the
// form names each scope by its last part.
const SCOPE_FIELDS = { "ussp.intents": "intents", "ussp.telemetry": "telemetry", "ussp.traffic": "traffic", "ussp.geo": "geo" } as const;

const createSchema = z
  .object({ intents: z.boolean(), telemetry: z.boolean(), traffic: z.boolean(), geo: z.boolean() })
  .refine((v) => SCOPES.some((s) => v[SCOPE_FIELDS[s]]), { message: "portal.clients.scopes_required", path: ["intents"] });

const bindSchema = z.object({
  serial: z.string().trim().min(1).max(64),
  class_label: z.enum(CLASS_LABELS).nullable(),
});

type Secret = Schemas["ClientSecret"];

function SecretOnce({ secret, onDone }: { secret: Secret; onDone(): void }) {
  const t = useAppT();
  return (
    <section role="alert" aria-labelledby="secret-heading" data-testid="client-secret" className="flex flex-col gap-2 rounded border-2 border-[var(--us-severity-warning)] p-3">
      <h2 id="secret-heading" className="m-0 text-base font-semibold">
        {t("portal.clients.secret_heading")}
      </h2>
      <p className="m-0 text-sm">{t("portal.clients.secret_once")}</p>
      <dl className="m-0 grid gap-2 sm:grid-cols-2">
        <Fact term={t("portal.clients.client_id")}>
          <code data-testid="secret-client-id">{secret.client_id}</code>
        </Fact>
        <Fact term={t("portal.clients.client_secret")}>
          <code data-testid="secret-value" className="break-all">
            {secret.client_secret}
          </code>
        </Fact>
        {secret.previous_valid_until !== undefined && (
          <Fact term={t("portal.clients.previous_valid_until")}>
            <Utc iso={secret.previous_valid_until} />
          </Fact>
        )}
      </dl>
      <Button variant="outline" size="sm" className="self-start" onClick={onDone}>
        {t("portal.clients.secret_done")}
      </Button>
    </section>
  );
}

function ClientRow({ c, operatorId, onChanged, onSecret }: { c: Schemas["ClientInfo"]; operatorId: string; onChanged(): void; onSecret(s: Secret): void }) {
  const t = useAppT();
  const api = useApi();
  const [error, setError] = useState<unknown>(null);
  const path = { operator_id: operatorId, client_id: c.client_id };
  return (
    <TableRow data-testid={`client-${c.client_id}`}>
      <TableCell className="font-mono text-xs">{c.client_id}</TableCell>
      <TableCell>{c.scopes.join(" ")}</TableCell>
      <TableCell>{c.status}</TableCell>
      <TableCell>
        <Utc iso={c.created_at} />
        {c.previous_valid_until !== undefined && (
          <p className="m-0 text-xs">
            {t("portal.clients.previous_valid_until")}: <Utc iso={c.previous_valid_until} />
          </p>
        )}
      </TableCell>
      <TableCell>
        <ul className="m-0 flex list-none flex-col gap-1 p-0" aria-label={t("portal.clients.serials")}>
          {c.serials.map((s) => (
            <li key={s.serial} className="flex items-center gap-2">
              <span className="font-mono text-xs">{s.serial}</span>
              <RequireRole anyOf={["operator_admin"]}>
                <Button
                  size="sm"
                  variant="outline"
                  aria-label={t("portal.clients.unbind_serial", { serial: s.serial })}
                  onClick={() => {
                    setError(null);
                    api
                      .DELETE("/v1/accounts/operators/{operator_id}/clients/{client_id}/serials/{serial}", {
                        params: { path: { ...path, serial: s.serial } },
                      })
                      .then(onChanged)
                      .catch(setError);
                  }}
                >
                  {t("portal.clients.unbind")}
                </Button>
              </RequireRole>
            </li>
          ))}
          {c.serials.length === 0 && <li className="text-xs text-[var(--us-text-muted)]">{t("portal.clients.no_serials")}</li>}
          {c.serials_truncated && <li className="text-xs">{t("portal.list.truncated")}</li>}
        </ul>
        <RequireRole anyOf={["operator_admin"]}>
          <details className="mt-2">
            <summary className="cursor-pointer text-sm">{t("portal.clients.bind_heading")}</summary>
            <Form
              schema={bindSchema}
              defaults={{ serial: "", class_label: null }}
              submitLabelKey="portal.clients.bind"
              onSubmit={async (v) => {
                await api.POST("/v1/accounts/operators/{operator_id}/clients/{client_id}/serials", {
                  params: { path },
                  body: v.class_label === null ? { serial: v.serial } : { serial: v.serial, class_label: v.class_label },
                });
                onChanged();
              }}
            >
              <TextField name="serial" labelKey="portal.clients.serial" required />
              <EnumField name="class_label" labelKey="portal.clients.class_label" hintKey="portal.clients.class_label_hint" values={CLASS_LABELS} i18nPrefix="portal.class_label" />
            </Form>
          </details>
        </RequireRole>
      </TableCell>
      <TableCell>
        <RequireRole anyOf={["operator_admin"]}>
          <Button
            size="sm"
            variant="outline"
            onClick={() => {
              setError(null);
              api
                .POST("/v1/accounts/operators/{operator_id}/clients/{client_id}/rotate", { params: { path } })
                .then(({ data }) => {
                  if (data !== undefined) onSecret(data);
                  onChanged();
                })
                .catch(setError);
            }}
          >
            {t("portal.clients.rotate")}
          </Button>
        </RequireRole>
        <ProblemNotice error={error} testId={`client-problem-${c.client_id}`} />
      </TableCell>
    </TableRow>
  );
}

function Clients({ operatorId }: { operatorId: string }) {
  const t = useAppT();
  const api = useApi();
  const [list, setList] = useState<Schemas["ClientList"] | null>(null);
  const [error, setError] = useState<unknown>(null);
  const [secret, setSecret] = useState<Secret | null>(null);
  const [tick, setTick] = useState(0);
  const load = useCallback(() => setTick((n) => n + 1), []);
  useEffect(() => {
    let live = true;
    api
      .GET("/v1/accounts/operators/{operator_id}/clients", { params: { path: { operator_id: operatorId } } })
      .then(({ data }) => {
        if (!live) return;
        setList(data ?? null);
        setError(null);
      })
      .catch((e: unknown) => live && setError(e));
    return () => {
      live = false;
    };
  }, [api, operatorId, tick]);
  return (
    <>
      {secret !== null && <SecretOnce secret={secret} onDone={() => setSecret(null)} />}
      <RequireRole anyOf={["operator_admin"]}>
        <section aria-labelledby="create-heading" className="rounded border border-[var(--us-border)] p-3">
          <h2 id="create-heading" className="m-0 mb-2 text-base font-semibold">
            {t("portal.clients.create_heading")}
          </h2>
          <Form
            schema={createSchema}
            defaults={{ intents: true, telemetry: true, traffic: true, geo: true }}
            submitLabelKey="portal.clients.create"
            onSubmit={async (v) => {
              const { data } = await api.POST("/v1/accounts/operators/{operator_id}/clients", {
                params: { path: { operator_id: operatorId } },
                body: { scopes: SCOPES.filter((s) => v[SCOPE_FIELDS[s]]) },
              });
              if (data !== undefined) setSecret(data);
              load();
            }}
          >
            {SCOPES.map((s) => (
              <CheckboxField key={s} name={SCOPE_FIELDS[s]} labelKey={`portal.scope.${SCOPE_FIELDS[s]}`} />
            ))}
          </Form>
        </section>
      </RequireRole>
      <ProblemNotice error={error} />
      {list !== null && (
        <section aria-labelledby="clients-list-heading">
          <h2 id="clients-list-heading" className="m-0 mb-2 text-base font-semibold">
            {t("portal.clients.list_heading", { n: list.clients.length })}
          </h2>
          {list.truncated && <p role="status">{t("portal.list.truncated")}</p>}
          {list.clients.length === 0 ? (
            <p role="status" data-testid="no-clients">
              {t("portal.clients.none")}
            </p>
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>{t("portal.clients.client_id")}</TableHead>
                  <TableHead>{t("portal.clients.scopes")}</TableHead>
                  <TableHead>{t("portal.clients.status")}</TableHead>
                  <TableHead>{t("portal.clients.created_at")}</TableHead>
                  <TableHead>{t("portal.clients.serials")}</TableHead>
                  <TableHead>{t("portal.clients.actions")}</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {list.clients.map((c) => (
                  <ClientRow key={c.client_id} c={c} operatorId={operatorId} onChanged={load} onSecret={setSecret} />
                ))}
              </TableBody>
            </Table>
          )}
        </section>
      )}
    </>
  );
}

export function ClientsPage() {
  const t = useAppT();
  const me = useMe();
  return (
    <section aria-labelledby="clients-heading" className="flex flex-col gap-3">
      <PageHeading id="clients-heading">{t("portal.clients.heading")}</PageHeading>
      <p className="m-0 text-sm text-[var(--us-text-muted)]">{t("portal.clients.note")}</p>
      <RequireSession>
        {me.kind === "failed" && <ProblemNotice error={me.error} />}
        {me.kind === "loading" && <p role="status">{t("portal.loading")}</p>}
        {me.kind === "loaded" &&
          (me.me.operator_id === undefined ? <p role="status">{t("portal.no_operator")}</p> : <Clients operatorId={me.me.operator_id} />)}
      </RequireSession>
    </section>
  );
}
