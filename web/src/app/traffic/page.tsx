import type { Metadata } from "next";
import { pageTitle } from "@/i18n/server";
import { TrafficPage } from "@/portal/live/TrafficPage";

export async function generateMetadata(): Promise<Metadata> {
  return { title: await pageTitle("portal.title.traffic") };
}

/** The intent this page follows, from ?intent= (one of the operator's; the API checks it). */
export default async function Page({ searchParams }: { searchParams: Promise<Record<string, string | string[] | undefined>> }) {
  const intent = (await searchParams)["intent"];
  return <TrafficPage intentId={typeof intent === "string" && intent !== "" ? intent : null} />;
}
