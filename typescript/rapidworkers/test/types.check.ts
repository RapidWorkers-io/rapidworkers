// Compile-only checks of the published typings (npm run typecheck).
import RapidWorkersDefault, { RapidWorkers, ToolError, type CreateAiTaskParams } from '../index.js';

const rw = new RapidWorkers({ apiKey: 'key' });
const same: RapidWorkers = new RapidWorkersDefault();

async function main(): Promise<void> {
    const account = await rw.getAccount();
    account.balance;
    await rw.listJobs();
    await rw.listJobs({ kind: 'ai_task', status: 'Active' });
    const quote = await rw.previewJobCost({ pay_rate: 0.1, quantity: 10, kind: 'ai_task' });
    const params: CreateAiTaskParams = {
        task_type: 'image_collection',
        title: 't',
        description: 'd',
        submission_instructions: 's',
        pay_rate: 0.1,
        quantity: 10,
        length_in_minutes: 2,
        countries: ['All'],
        quote_id: String(quote.quote_id),
        confirm: true,
        submission_fields: [{ type: 'short_text', label: 'Your city' }],
    };
    await rw.createAiTask(params);
    await rw.callTool('get_account');
    try {
        await same.getJob({ job_id: 'x' });
    } catch (error) {
        if (error instanceof ToolError) error.details;
    }
    // @ts-expect-error confirm must be literally true
    await rw.cancelJob({ job_id: 'x', confirm: false });
    // @ts-expect-error unknown enum value
    await rw.listJobs({ kind: 'campaign' });
    // @ts-expect-error required job_id
    await rw.getJob({});
}

void main;
