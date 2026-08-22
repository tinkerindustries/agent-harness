// SkillBadge marks a tool row as the model taking a skill: a Read whose
// target is one of the SKILL.md paths this run's catalogue offered
// (api/skillCatalogue.ts, toolArgs.ts's skillOpened).
//
// It exists because that act has no other trace. The catalogue block at the
// top of a transcript says what was on offer and every skill after that is an
// ordinary Read among the hundreds a run makes — so "the model followed the
// test-runner skill" was a fact you could only recover by reading paths, on
// the one row out of two hundred that carried one.
//
// The badge says "skill" and not the skill's name: the name is right beside
// it in the row's own target, and a row that has to fit a tool name, a path,
// a stat and this cannot spend a skill name's width on saying it twice. The
// name goes in the title for the case the path is truncated to fit.
export function SkillBadge({ name }: { name: string }) {
  return (
    <span
      className="flex-none rounded-full border border-border bg-muted px-1.5 py-px text-micro text-muted-foreground uppercase tracking-[var(--label-tracking)]"
      title={`Opened the ${name} skill offered to this run`}
    >
      skill
    </span>
  );
}
