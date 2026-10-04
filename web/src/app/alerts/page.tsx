import type { Metadata } from "next";
import { pageTitle } from "@/i18n/server";
import { AlertsPage } from "@/portal/live/AlertsPage";

export async function generateMetadata(): Promise<Metadata> {
  return { title: await pageTitle("portal.title.alerts") };
}

/** The intent this page follows, from ?intent= (one of the operator's; the API checks it). */
export default async function Page({ searchParams }: { searchParams: Promise<Record<string, string | string[] | undefined>> }) {
  const intent = (await searchParams)["intent"];
  return <AlertsPage intentId={typeof intent === "string" && intent !== "" ? intent : null} />;
}
